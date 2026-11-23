package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- model metadata (context window, price) ------------------------------------------

// ModelMeta is whatever the provider's /models list told us about a model.
// Prices are USD per 1M tokens; Priced is false when the provider doesn't publish them.
type ModelMeta struct {
	Ctx    int     `json:"ctx,omitempty"`
	In     float64 `json:"in,omitempty"`
	Out    float64 `json:"out,omitempty"`
	Priced bool    `json:"priced,omitempty"`
}

type modelInfo struct {
	ID   string
	Meta ModelMeta
}

func anyNum(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// parseMeta digs context length and pricing out of the shapes the common providers use.
func parseMeta(m map[string]any) ModelMeta {
	var mm ModelMeta
	for _, k := range []string{"context_length", "context_window", "max_context_length", "max_model_len", "context_size", "inputTokenLimit", "max_input_tokens"} {
		if f, ok := anyNum(m[k]); ok && f > 0 {
			mm.Ctx = int(f)
			break
		}
	}
	if top, ok := m["top_provider"].(map[string]any); ok && mm.Ctx == 0 {
		if f, ok := anyNum(top["context_length"]); ok {
			mm.Ctx = int(f)
		}
	}
	if p, ok := m["pricing"].(map[string]any); ok {
		// OpenRouter style: dollars per token, as strings.
		if a, ok := anyNum(p["prompt"]); ok {
			b, _ := anyNum(p["completion"])
			mm.In, mm.Out, mm.Priced = a*1e6, b*1e6, true
		} else if a, ok := anyNum(p["input"]); ok { // Together style: dollars per 1M
			b, _ := anyNum(p["output"])
			mm.In, mm.Out, mm.Priced = a, b, true
		}
		if mm.In < 0 || mm.Out < 0 { // OpenRouter uses -1 for "varies"
			mm.In, mm.Out, mm.Priced = 0, 0, false
		}
	}
	return mm
}

// ---- persistent aggregates ----------------------------------------------------------------

type Agg struct {
	Req  int     `json:"r"`
	Err  int     `json:"e,omitempty"`
	In   int     `json:"i,omitempty"`
	Out  int     `json:"o,omitempty"`
	Cost float64 `json:"c,omitempty"`
}

func (a *Agg) add(b Agg) {
	a.Req += b.Req
	a.Err += b.Err
	a.In += b.In
	a.Out += b.Out
	a.Cost += b.Cost
}

// Stats keeps daily totals per provider/model/key (so lifetime numbers survive the
// request log being trimmed) and request counts per hour for the usage heatmap.
type Stats struct {
	mu    sync.Mutex
	path  string
	dirty bool
	d     statsData
}

type statsData struct {
	First int64                      `json:"first"`
	Days  map[string]map[string]*Agg `json:"days"`  // "2026-11-23" -> "provider\tmodel\tkey" -> totals
	Hours map[string]int             `json:"hours"` // unix hour -> requests
}

func OpenStats(path string, seed []LogEntry, cost func(LogEntry) float64) *Stats {
	s := &Stats{path: path, d: statsData{Days: map[string]map[string]*Agg{}, Hours: map[string]int{}}}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &s.d) == nil {
		if s.d.Days == nil {
			s.d.Days = map[string]map[string]*Agg{}
		}
		if s.d.Hours == nil {
			s.d.Hours = map[string]int{}
		}
		return s
	}
	for i := len(seed) - 1; i >= 0; i-- { // seed from the existing request log, oldest first
		s.Add(seed[i], cost(seed[i]))
	}
	return s
}

func (s *Stats) Add(e LogEntry, cost float64) {
	if e.Provider == "" {
		return
	}
	t := time.UnixMilli(e.T).UTC()
	day := t.Format("2006-01-02")
	model := e.Model
	if model == "" {
		model = "(none)"
	}
	k := e.Provider + "\t" + model + "\t" + e.Key
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.First == 0 || e.T < s.d.First {
		s.d.First = e.T
	}
	m := s.d.Days[day]
	if m == nil {
		m = map[string]*Agg{}
		s.d.Days[day] = m
	}
	a := m[k]
	if a == nil {
		a = &Agg{}
		m[k] = a
	}
	a.Req++
	if e.Status >= 400 || e.Status == 0 {
		a.Err++
	}
	a.In += e.In
	a.Out += e.Out
	a.Cost += cost
	s.d.Hours[strconv.FormatInt(e.T/3600000, 10)]++
	s.dirty = true
}

func (s *Stats) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	cut := time.Now().UTC().AddDate(0, 0, -400).Format("2006-01-02")
	for d := range s.d.Days {
		if d < cut {
			delete(s.d.Days, d)
		}
	}
	hcut := time.Now().Add(-15*24*time.Hour).UnixMilli() / 3600000
	for h := range s.d.Hours {
		if n, _ := strconv.ParseInt(h, 10, 64); n < hcut {
			delete(s.d.Hours, h)
		}
	}
	b, err := json.Marshal(s.d)
	if err == nil && os.WriteFile(s.path, b, 0o600) == nil {
		s.dirty = false
	}
}

func (s *Stats) Clear() {
	s.mu.Lock()
	s.d = statsData{Days: map[string]map[string]*Agg{}, Hours: map[string]int{}}
	s.dirty = true
	s.mu.Unlock()
	s.Flush()
}

// ---- server-side helpers --------------------------------------------------------------------

func (s *Server) metaFor(provider, model string) (ModelMeta, bool) {
	s.mcMu.Lock()
	defer s.mcMu.Unlock()
	m, ok := s.meta[provider+"/"+model]
	return m, ok
}

func (s *Server) costOf(e LogEntry) float64 {
	if e.In == 0 && e.Out == 0 {
		return 0
	}
	m, ok := s.metaFor(e.Provider, e.Model)
	if !ok || !m.Priced {
		return 0
	}
	return (float64(e.In)*m.In + float64(e.Out)*m.Out) / 1e6
}

type catItem struct {
	Provider string
	Model    string
	Meta     ModelMeta
}

// catalog returns every model the configured providers advertise, cached for 5 minutes.
func (s *Server) catalog(ctx context.Context) []catItem {
	s.mcMu.Lock()
	if s.cat != nil && time.Since(s.catAt) < 5*time.Minute {
		c := s.cat
		s.mcMu.Unlock()
		return c
	}
	s.mcMu.Unlock()
	s.buildModels(ctx) // fills s.cat and s.meta
	s.mcMu.Lock()
	defer s.mcMu.Unlock()
	return s.cat
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

type modelStat struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Ctx       int       `json:"ctx,omitempty"`
	In        float64   `json:"in,omitempty"`
	Out       float64   `json:"out,omitempty"`
	Priced    bool      `json:"priced,omitempty"`
	Listed    bool      `json:"listed"`
	Req       int       `json:"req"`
	Err       int       `json:"err"`
	TtftP50   float64   `json:"ttft_p50,omitempty"`
	TtftP90   float64   `json:"ttft_p90,omitempty"`
	Tps       float64   `json:"tps,omitempty"`
	Last      int64     `json:"last,omitempty"`
	TokensIn  int       `json:"tokens_in,omitempty"`
	TokensOut int       `json:"tokens_out,omitempty"`
	Up        [][2]int  `json:"up,omitempty"` // per hour, oldest first: [ok, failed]
	ttft      []float64 `json:"-"`
	tps       []float64 `json:"-"`
}

func (s *Server) apiModelStats(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if r.URL.Query().Get("hours") == "72" {
		hours = 72
	}
	now := time.Now()
	cutoff := now.Add(-time.Duration(hours) * time.Hour).UnixMilli()
	end := (now.UnixMilli()/3600000 + 1) * 3600000

	by := map[string]*modelStat{}
	get := func(p, m string) *modelStat {
		id := p + "/" + m
		ms := by[id]
		if ms == nil {
			ms = &modelStat{ID: id, Provider: p, Model: m}
			by[id] = ms
		}
		return ms
	}
	for _, c := range s.catalog(r.Context()) {
		ms := get(c.Provider, c.Model)
		ms.Listed = true
		ms.Ctx, ms.In, ms.Out, ms.Priced = c.Meta.Ctx, c.Meta.In, c.Meta.Out, c.Meta.Priced
	}
	for _, l := range s.logs.Recent(logCap) { // newest first
		if l.T < cutoff {
			break
		}
		if l.Model == "" || l.Provider == "" {
			continue
		}
		ms := get(l.Provider, l.Model)
		if m, ok := s.metaFor(l.Provider, l.Model); ok && !ms.Listed {
			ms.Ctx, ms.In, ms.Out, ms.Priced = m.Ctx, m.In, m.Out, m.Priced
		}
		bad := l.Status >= 400 || l.Status == 0
		ms.Req++
		if bad {
			ms.Err++
		}
		if l.T > ms.Last {
			ms.Last = l.T
		}
		ms.TokensIn += l.In
		ms.TokensOut += l.Out
		if !bad && l.Ttft > 0 {
			ms.ttft = append(ms.ttft, float64(l.Ttft))
		}
		if !bad && l.Stream && l.Out > 0 && l.Ms-l.Ttft > 300 {
			ms.tps = append(ms.tps, float64(l.Out)/(float64(l.Ms-l.Ttft)/1000))
		}
		if ms.Up == nil {
			ms.Up = make([][2]int, hours)
		}
		i := hours - 1 - int((end-l.T)/3600000)
		if i >= 0 && i < hours {
			if bad {
				ms.Up[i][1]++
			} else {
				ms.Up[i][0]++
			}
		}
	}
	out := make([]*modelStat, 0, len(by))
	for _, ms := range by {
		sort.Float64s(ms.ttft)
		sort.Float64s(ms.tps)
		ms.TtftP50, ms.TtftP90 = pct(ms.ttft, .5), pct(ms.ttft, .9)
		ms.Tps = pct(ms.tps, .5)
		out = append(out, ms)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Req != out[j].Req {
			return out[i].Req > out[j].Req
		}
		return out[i].ID < out[j].ID
	})
	total, reqs := 0, 0
	for _, m := range out {
		if m.Listed || m.Req > 0 {
			total++
		}
		reqs += m.Req
	}
	jsonOut(w, map[string]any{"hours": hours, "models": out, "total": total, "requests": reqs, "at": now.UnixMilli()})
}

// ---- /api/usage -----------------------------------------------------------------------------------

type usageRow struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model,omitempty"`
	Key      string  `json:"key,omitempty"`
	Req      int     `json:"req"`
	Err      int     `json:"err"`
	In       int     `json:"in"`
	Out      int     `json:"out"`
	Cost     float64 `json:"cost"`
}

func (s *Server) apiUsage(w http.ResponseWriter, r *http.Request) {
	st := s.stats
	st.mu.Lock()
	defer st.mu.Unlock()

	today := time.Now().UTC()
	var life Agg
	models := map[string]bool{}
	type dayRow struct {
		Day  string  `json:"day"`
		Req  int     `json:"req"`
		Err  int     `json:"err"`
		In   int     `json:"in"`
		Out  int     `json:"out"`
		Cost float64 `json:"cost"`
	}
	daily := []dayRow{}
	byModel, byKey := map[string]*usageRow{}, map[string]*usageRow{}
	activeDays := map[string]bool{}

	days := make([]string, 0, len(st.d.Days))
	for d := range st.d.Days {
		days = append(days, d)
	}
	sort.Strings(days)
	weekCut := today.AddDate(0, 0, -6).Format("2006-01-02")
	byDay := map[string]*dayRow{}
	for _, d := range days {
		dr := &dayRow{Day: d}
		for k, a := range st.d.Days[d] {
			parts := strings.SplitN(k, "\t", 3)
			for len(parts) < 3 {
				parts = append(parts, "")
			}
			life.add(*a)
			models[parts[0]+"/"+parts[1]] = true
			dr.Req += a.Req
			dr.Err += a.Err
			dr.In += a.In
			dr.Out += a.Out
			dr.Cost += a.Cost
			if a.Req > 0 {
				activeDays[d] = true
			}
			if d >= weekCut {
				mk := parts[0] + "/" + parts[1]
				m := byModel[mk]
				if m == nil {
					m = &usageRow{Provider: parts[0], Model: parts[1]}
					byModel[mk] = m
				}
				kk := parts[0] + "\t" + parts[2]
				kr := byKey[kk]
				if kr == nil {
					kr = &usageRow{Provider: parts[0], Key: parts[2]}
					byKey[kk] = kr
				}
				for _, u := range []*usageRow{m, kr} {
					u.Req += a.Req
					u.Err += a.Err
					u.In += a.In
					u.Out += a.Out
					u.Cost += a.Cost
				}
			}
		}
		byDay[d] = dr
	}
	for i := 89; i >= 0; i-- { // fill gaps so the chart has a steady x axis
		d := today.AddDate(0, 0, -i).Format("2006-01-02")
		if dr, ok := byDay[d]; ok {
			daily = append(daily, *dr)
		} else {
			daily = append(daily, dayRow{Day: d})
		}
	}
	streak := 0
	for i := 0; ; i++ {
		d := today.AddDate(0, 0, -i).Format("2006-01-02")
		if activeDays[d] {
			streak++
		} else if i == 0 { // today may simply not have started yet
			continue
		} else {
			break
		}
	}
	hours := map[string]int{}
	hcut := today.Add(-7*24*time.Hour).UnixMilli() / 3600000
	for h, n := range st.d.Hours {
		if v, _ := strconv.ParseInt(h, 10, 64); v >= hcut {
			hours[h] = n
		}
	}
	rows := func(m map[string]*usageRow) []*usageRow {
		o := make([]*usageRow, 0, len(m))
		for _, v := range m {
			o = append(o, v)
		}
		sort.Slice(o, func(i, j int) bool { return o[i].Req > o[j].Req })
		if len(o) > 25 {
			o = o[:25]
		}
		return o
	}
	jsonOut(w, map[string]any{
		"lifetime": map[string]any{"req": life.Req, "err": life.Err, "in": life.In, "out": life.Out, "cost": life.Cost,
			"first": st.d.First, "streak": streak, "models": len(models)},
		"daily": daily, "hours": hours, "by_model": rows(byModel), "by_key": rows(byKey),
	})
}
