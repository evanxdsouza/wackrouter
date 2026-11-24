package main

import (
	"context"
	"sort"
	"strings"
)

// rc is one place a model can be served from: a provider and that provider's own id for it.
type rc struct {
	Slug string
	Up   string
}

// canon reduces a model id to the name every provider shares:
// "google/gemma-4-31b-it", "models/gemma-4-31b-it" and "gemma-4-31b-it:free" are all one model.
func canon(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.TrimSuffix(id, ":free")
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// buildRoutes groups every provider's models by canonical name. It returns the candidates
// for each name and the id to show for it (the longest vendor-prefixed spelling).
func buildRoutes(flat []rc, provs []apiProvider) (map[string][]rc, map[string]string) {
	order := map[string]int{}
	for i, p := range provs {
		order[p.ID] = i
	}
	route := map[string][]rc{}
	for _, c := range flat {
		k := canon(c.Up)
		if k == "" {
			continue
		}
		dup := -1
		for i, e := range route[k] {
			if e.Slug == c.Slug {
				dup = i
				break
			}
		}
		if dup >= 0 { // one entry per provider; prefer the :free spelling
			if strings.HasSuffix(c.Up, ":free") && !strings.HasSuffix(route[k][dup].Up, ":free") {
				route[k][dup] = c
			}
			continue
		}
		route[k] = append(route[k], c)
	}
	disp := map[string]string{}
	for k, cs := range route {
		sort.SliceStable(cs, func(i, j int) bool { return order[cs[i].Slug] < order[cs[j].Slug] })
		best := ""
		for _, c := range cs {
			id := strings.TrimSuffix(c.Up, ":free")
			id = strings.TrimPrefix(id, "models/")
			if strings.Contains(id, "/") {
				// keep the vendor/name spelling most providers agree on: shortest vendor form
				if best == "" || !strings.Contains(best, "/") || len(id) < len(best) {
					best = id
				}
			} else if best == "" {
				best = id
			}
		}
		disp[k] = best
	}
	return route, disp
}

// candidates returns every provider that serves the model, building the table on first use.
func (s *Server) candidates(ctx context.Context, model string) []rc {
	s.mcMu.Lock()
	r := s.route
	s.mcMu.Unlock()
	if r == nil {
		s.buildModels(ctx)
		s.mcMu.Lock()
		r = s.route
		s.mcMu.Unlock()
	}
	cs := r[canon(model)]
	out := make([]rc, 0, len(cs))
	for _, c := range cs {
		if p, ok := s.store.GetProvider(c.Slug); ok && p.Enabled {
			out = append(out, c)
		}
	}
	return out
}

// rank puts the providers that have been working (and answering fastest) lately first.
func (s *Server) rank(cs []rc, model string) []rc {
	if len(cs) < 2 {
		return cs
	}
	type sc struct {
		ok, n int
		ttft  []float64
	}
	by := map[string]*sc{}
	for _, c := range cs {
		by[c.Slug] = &sc{}
	}
	log := s.logs.Recent(4000)
	for i := len(log) - 1; i >= 0; i-- {
		l := log[i]
		x := by[l.Provider]
		if x == nil || canon(l.Model) != canon(model) || x.n >= 40 || l.Status == 499 {
			continue
		}
		x.n++
		if l.Status < 400 {
			x.ok++
			if l.Ttft > 0 {
				x.ttft = append(x.ttft, float64(l.Ttft))
			}
		}
	}
	rate := func(slug string) float64 {
		x := by[slug]
		if x.n == 0 {
			return 0.7 // untried: after proven ones, before known-bad ones
		}
		return float64(int(float64(x.ok)/float64(x.n)*10)) / 10
	}
	lat := func(slug string) float64 {
		x := by[slug]
		if len(x.ttft) == 0 {
			return 4000
		}
		sort.Float64s(x.ttft)
		return pct(x.ttft, .5)
	}
	out := append([]rc(nil), cs...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rate(out[i].Slug), rate(out[j].Slug)
		if ri != rj {
			return ri > rj
		}
		return lat(out[i].Slug) < lat(out[j].Slug)
	})
	return out
}
