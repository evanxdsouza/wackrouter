package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A Sink ("drain") receives a copy of every request log entry.
// Metadata only: prompts, responses and keys are never sent.
type Sink struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`   // "logflare" | "webhook"
	URL     string `json:"url"`    // Logflare base URL, or the full webhook URL
	Source  string `json:"source"` // Logflare source UUID or name
	Secret  string `json:"secret"` // encrypted: Logflare API key, or webhook Authorization header value
	Hint    string `json:"hint"`
	Enabled bool   `json:"enabled"`
}

type SinkStatus struct {
	LastOK, LastFail int64
	LastErr          string
	Sent, Failed     int64
}

type SinkDec struct {
	Sink
	Key string
}

type apiSink struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Source   string `json:"source"`
	Hint     string `json:"hint"`
	Enabled  bool   `json:"enabled"`
	LastOK   int64  `json:"last_ok"`
	LastFail int64  `json:"last_fail"`
	LastErr  string `json:"last_err"`
	Sent     int64  `json:"sent"`
	Failed   int64  `json:"failed"`
}

// ---- store methods ---------------------------------------------------------------

func last4(s string) string {
	if len(s) > 4 {
		return s[len(s)-4:]
	}
	return s
}

func (s *Store) UpsertSink(k Sink, secret string) error {
	if !slugRe.MatchString(k.ID) {
		return errors.New("id must be lowercase letters, digits or dashes (max 32)")
	}
	if k.Type != "logflare" && k.Type != "webhook" {
		return errors.New("type must be logflare or webhook")
	}
	k.URL = strings.TrimRight(strings.TrimSpace(k.URL), "/")
	if k.Type == "logflare" && k.URL == "" {
		k.URL = "https://api.logflare.app"
	}
	if !strings.HasPrefix(k.URL, "http://") && !strings.HasPrefix(k.URL, "https://") {
		return errors.New("URL must start with http:// or https://")
	}
	k.Source = strings.TrimSpace(k.Source)
	if k.Type == "logflare" && k.Source == "" {
		return errors.New("the Logflare source (UUID or name) is required")
	}
	if k.Type == "webhook" {
		k.Source = ""
	}
	if strings.TrimSpace(k.Name) == "" {
		k.Name = k.ID
	}
	secret = strings.TrimSpace(secret)

	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i := range s.D.Sinks {
		if s.D.Sinks[i].ID == k.ID {
			idx = i
		}
	}
	if idx >= 0 {
		k.Secret, k.Hint = s.D.Sinks[idx].Secret, s.D.Sinks[idx].Hint
	}
	if secret != "" {
		k.Secret, k.Hint = s.enc(secret), last4(secret)
	}
	if k.Type == "logflare" && k.Secret == "" {
		return errors.New("the Logflare API key is required")
	}
	if idx >= 0 {
		s.D.Sinks[idx] = k
	} else {
		s.D.Sinks = append(s.D.Sinks, k)
	}
	return s.save()
}

func (s *Store) DeleteSink(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.D.Sinks {
		if s.D.Sinks[i].ID == id {
			s.D.Sinks = append(s.D.Sinks[:i], s.D.Sinks[i+1:]...)
			delete(s.ss, id)
			return s.save()
		}
	}
	return errors.New("not found")
}

func (s *Store) SetSinkEnabled(id string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.D.Sinks {
		if s.D.Sinks[i].ID == id {
			s.D.Sinks[i].Enabled = on
			return s.save()
		}
	}
	return errors.New("not found")
}

func (s *Store) sinkDec(k Sink) SinkDec {
	d := SinkDec{Sink: k}
	if k.Secret != "" {
		d.Key, _ = s.dec(k.Secret)
	}
	return d
}

func (s *Store) SinkByID(id string) (SinkDec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.D.Sinks {
		if k.ID == id {
			return s.sinkDec(k), true
		}
	}
	return SinkDec{}, false
}

func (s *Store) ActiveSinks() []SinkDec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []SinkDec
	for _, k := range s.D.Sinks {
		if k.Enabled {
			out = append(out, s.sinkDec(k))
		}
	}
	return out
}

func (s *Store) NoteSink(id string, err error, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.ss[id]
	if err != nil {
		st.LastFail, st.LastErr, st.Failed = time.Now().Unix(), err.Error(), st.Failed+int64(n)
	} else {
		st.LastOK, st.LastErr, st.Sent = time.Now().Unix(), "", st.Sent+int64(n)
	}
	s.ss[id] = st
}

func (s *Store) sinkSnapshot() []apiSink { // caller holds read lock
	out := []apiSink{}
	for _, k := range s.D.Sinks {
		st := s.ss[k.ID]
		out = append(out, apiSink{k.ID, k.Name, k.Type, k.URL, k.Source, k.Hint, k.Enabled, st.LastOK, st.LastFail, st.LastErr, st.Sent, st.Failed})
	}
	return out
}

// ---- shipper -------------------------------------------------------------------------

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Shipper struct {
	st     *Store
	client *http.Client
	ch     chan LogEntry
}

func NewShipper(st *Store, client *http.Client) *Shipper {
	sh := &Shipper{st: st, client: client, ch: make(chan LogEntry, 2000)}
	go sh.loop()
	return sh
}

// Enqueue never blocks the request path; if the queue is full the event is dropped.
func (sh *Shipper) Enqueue(e LogEntry) {
	select {
	case sh.ch <- e:
	default:
	}
}

func (sh *Shipper) loop() {
	var batch []LogEntry
	tick := time.NewTicker(2 * time.Second)
	for {
		select {
		case e := <-sh.ch:
			batch = append(batch, e)
			if len(batch) >= 50 {
				sh.flush(batch)
				batch = nil
			}
		case <-tick.C:
			if len(batch) > 0 {
				sh.flush(batch)
				batch = nil
			}
		}
	}
}

func (sh *Shipper) flush(batch []LogEntry) {
	for _, sk := range sh.st.ActiveSinks() {
		go func(sk SinkDec) {
			err := sh.post(sk, batch)
			if err != nil {
				time.Sleep(time.Second)
				err = sh.post(sk, batch)
			}
			sh.st.NoteSink(sk.ID, err, len(batch))
		}(sk)
	}
}

func eventMessage(e LogEntry) string {
	return fmt.Sprintf("%s %s/%s %d %dms", e.Path, e.Provider, e.Model, e.Status, e.Ms)
}

func eventFields(e LogEntry) map[string]any {
	return map[string]any{
		"who": e.Who, "provider": e.Provider, "model": e.Model, "path": e.Path, "key": e.Key,
		"status": e.Status, "latency_ms": e.Ms, "tokens_in": e.In, "tokens_out": e.Out, "stream": e.Stream, "error": e.Err,
	}
}

func eventTime(e LogEntry) string { return time.UnixMilli(e.T).UTC().Format(time.RFC3339Nano) }

func buildSinkRequest(ctx context.Context, sk SinkDec, evs []LogEntry) (*http.Request, error) {
	var target string
	var payload any
	hdr := http.Header{"Content-Type": {"application/json"}}
	switch sk.Type {
	case "logflare":
		q := url.Values{}
		if uuidRe.MatchString(sk.Source) {
			q.Set("source", sk.Source)
		} else {
			q.Set("source_name", sk.Source)
		}
		target = sk.URL + "/api/logs?" + q.Encode()
		batch := make([]map[string]any, 0, len(evs))
		for _, e := range evs {
			batch = append(batch, map[string]any{
				"message": eventMessage(e), "timestamp": eventTime(e),
				"metadata": map[string]any{"wackrouter": eventFields(e)},
			})
		}
		payload = map[string]any{"batch": batch}
		hdr.Set("X-API-KEY", sk.Key)
	default: // webhook: a JSON array of flat events
		target = sk.URL
		arr := make([]map[string]any, 0, len(evs))
		for _, e := range evs {
			f := eventFields(e)
			f["message"], f["timestamp"] = eventMessage(e), eventTime(e)
			arr = append(arr, f)
		}
		payload = arr
		if sk.Key != "" {
			hdr.Set("Authorization", sk.Key)
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = hdr
	return req, nil
}

func (sh *Shipper) post(sk SinkDec, evs []LogEntry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := buildSinkRequest(ctx, sk, evs)
	if err != nil {
		return err
	}
	resp, err := sh.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = "no response body"
		}
		return errors.New("HTTP " + strconv.Itoa(resp.StatusCode) + ": " + msg)
	}
	return nil
}

// Test sends one synthetic event and reports the result.
func (sh *Shipper) Test(id string) (int64, error) {
	sk, ok := sh.st.SinkByID(id)
	if !ok {
		return 0, errors.New("not found")
	}
	start := time.Now()
	err := sh.post(sk, []LogEntry{{T: start.UnixMilli(), Who: "wackrouter", Provider: "test", Model: "ping", Path: "/test", Status: 200}})
	sh.st.NoteSink(id, err, 1)
	return time.Since(start).Milliseconds(), err
}
