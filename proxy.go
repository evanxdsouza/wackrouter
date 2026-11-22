package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- request log -------------------------------------------------------------

type LogEntry struct {
	T        int64  `json:"t"` // unix ms
	Who      string `json:"who"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Path     string `json:"path"`
	Key      string `json:"key"`
	Status   int    `json:"status"`
	Ms       int64  `json:"ms"`
	In       int    `json:"in"`
	Out      int    `json:"out"`
	Stream   bool   `json:"stream"`
	Err      string `json:"err,omitempty"`
}

const logCap = 2000

type Logs struct {
	mu    sync.Mutex
	items []LogEntry
	path  string
	hook  func(LogEntry) // called after each entry is stored (used by drains)
}

func OpenLogs(path string) *Logs {
	l := &Logs{path: path}
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range bytes.Split(b, []byte("\n")) {
			var e LogEntry
			if len(line) > 0 && json.Unmarshal(line, &e) == nil {
				l.items = append(l.items, e)
			}
		}
		if len(l.items) > logCap {
			l.items = l.items[len(l.items)-logCap:]
		}
		l.rewrite()
	}
	return l
}

func (l *Logs) rewrite() {
	var buf bytes.Buffer
	for _, e := range l.items {
		b, _ := json.Marshal(e)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	os.WriteFile(l.path, buf.Bytes(), 0o600)
}

func (l *Logs) Add(e LogEntry) {
	l.add(e)
	if l.hook != nil {
		l.hook(e)
	}
}

func (l *Logs) add(e LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, e)
	if len(l.items) > logCap+200 {
		l.items = append([]LogEntry(nil), l.items[len(l.items)-logCap:]...)
		l.rewrite()
		return
	}
	if f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
		f.Close()
	}
}

func (l *Logs) Recent(n int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.items) {
		n = len(l.items)
	}
	out := make([]LogEntry, n)
	for i := 0; i < n; i++ { // newest first
		out[i] = l.items[len(l.items)-1-i]
	}
	return out
}

func (l *Logs) Clear() {
	l.mu.Lock()
	l.items = nil
	os.WriteFile(l.path, nil, 0o600)
	l.mu.Unlock()
}

// ---- usage sniffing -------------------------------------------------------------

type capture struct{ head, tail []byte }

func (c *capture) add(p []byte) {
	if len(c.head) < 8192 {
		n := 8192 - len(c.head)
		if n > len(p) {
			n = len(p)
		}
		c.head = append(c.head, p[:n]...)
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > 16384 {
		c.tail = append([]byte(nil), c.tail[len(c.tail)-8192:]...)
	}
}

var (
	reIn  = regexp.MustCompile(`"(?:prompt_tokens|input_tokens)"\s*:\s*(\d+)`)
	reOut = regexp.MustCompile(`"(?:completion_tokens|output_tokens)"\s*:\s*(\d+)`)
)

func lastInt(re *regexp.Regexp, data []byte) int {
	m := re.FindAllSubmatch(data, -1)
	if len(m) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(string(m[len(m)-1][1]))
	return n
}

func (c *capture) usage() (in, out int) {
	all := append(append([]byte(nil), c.head...), c.tail...)
	return lastInt(reIn, all), lastInt(reOut, all)
}

var errMsgRe = regexp.MustCompile(`"message"\s*:\s*"((?:[^"\\]|\\.)*)"`)

func (c *capture) errText() string {
	if m := errMsgRe.FindSubmatch(c.head); m != nil {
		return string(m[1])
	}
	s := strings.TrimSpace(string(c.head))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// ---- helpers --------------------------------------------------------------------

var hop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

func oaiErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "wackrouter_error", "code": code}})
}

func clientToken(r *http.Request) string {
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	return r.Header.Get("X-Goog-Api-Key")
}

func setAuth(h http.Header, p Provider, extra map[string]string, rest, key string) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("X-Goog-Api-Key")
	switch p.Kind {
	case "anthropic":
		h.Set("X-Api-Key", key)
		if h.Get("Anthropic-Version") == "" {
			h.Set("Anthropic-Version", "2023-06-01")
		}
		if strings.HasPrefix(rest, "/chat/completions") { // OpenAI-compat endpoint
			h.Set("Authorization", "Bearer "+key)
		}
	case "custom":
		h.Set(p.AuthHeader, p.AuthPrefix+key)
	default:
		h.Set("Authorization", "Bearer "+key)
	}
	for k, v := range extra { // provider-level custom headers win over the client's
		h.Set(k, v)
	}
}

func peekModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &m)
	return m.Model
}

// ---- entry points -----------------------------------------------------------------

func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	who, ok := s.store.AuthToken(clientToken(r))
	if !ok {
		msg := "Invalid or missing Wackrouter token. Send it as 'Authorization: Bearer wr-…'."
		if !s.store.HasTokens() {
			msg = "No access tokens exist yet. Create one in the Wackrouter UI under Access."
		}
		oaiErr(w, 401, "invalid_api_key", msg)
		return
	}
	s.serve(w, r, who)
}

// /p/{provider}/... forwards untouched to that provider (native APIs, e.g. Anthropic /v1/messages).
func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	who, ok := s.store.AuthToken(clientToken(r))
	if !ok {
		oaiErr(w, 401, "invalid_api_key", "Invalid or missing Wackrouter token.")
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/p/"), "/", 2)
	rest := "/"
	if len(parts) == 2 {
		rest = "/" + parts[1]
	}
	if _, ok := s.store.GetProvider(parts[0]); !ok {
		oaiErr(w, 404, "unknown_provider", "No provider with id '"+parts[0]+"'.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		oaiErr(w, 413, "body_too_large", "Request body too large.")
		return
	}
	if rest == "/v1" || strings.HasPrefix(rest, "/v1/") {
		rest = strings.TrimPrefix(rest, "/v1")
	}
	s.forward(w, r, who, parts[0], rest, body, peekModel(body))
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, who string) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1")
	if r.Method == http.MethodGet && rest == "/models" {
		s.listModels(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		oaiErr(w, 413, "body_too_large", "Request body too large.")
		return
	}
	slug, model := "", ""
	if len(body) > 0 && strings.Contains(r.Header.Get("Content-Type"), "json") {
		var m map[string]json.RawMessage
		if json.Unmarshal(body, &m) == nil {
			var mv string
			if raw, ok := m["model"]; ok {
				json.Unmarshal(raw, &mv)
			}
			if mv != "" {
				slug, model = s.store.Resolve(mv)
				if model != mv {
					m["model"], _ = json.Marshal(model)
					body, _ = json.Marshal(m)
				}
			}
		}
	}
	if slug == "" {
		if slug = s.store.DefaultSlug(); slug == "" {
			oaiErr(w, 400, "no_provider", "No provider configured. Add one in the Wackrouter UI.")
			return
		}
	}
	if q := r.URL.RawQuery; q != "" {
		rest += "?" + q
	}
	s.forward(w, r, who, slug, rest, body, model)
}

// ---- the forwarder ---------------------------------------------------------------------

func (s *Server) forward(w http.ResponseWriter, r *http.Request, who, slug, rest string, body []byte, model string) {
	start := time.Now()
	e := LogEntry{T: start.UnixMilli(), Who: who, Provider: slug, Model: model, Path: strings.SplitN(rest, "?", 2)[0]}
	defer func() {
		e.Ms = time.Since(start).Milliseconds()
		s.logs.Add(e)
	}()

	prov, ok := s.store.GetProvider(slug)
	if !ok || !prov.Enabled {
		e.Status, e.Err = 404, "provider unavailable"
		oaiErr(w, 404, "unknown_provider", "Provider '"+slug+"' is not available.")
		return
	}
	keys, err := s.store.PickKeys(slug)
	if err != nil {
		e.Status, e.Err = 503, err.Error()
		oaiErr(w, 503, "no_keys", slug+": "+err.Error())
		return
	}

	target := prov.BaseURL + rest
	extra := s.store.HeadersFor(prov)
	var resp *http.Response
	var lastErr error
	for i, k := range keys {
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		for name, vv := range r.Header {
			switch http.CanonicalHeaderKey(name) {
			case "Host", "Content-Length", "Accept-Encoding", "Cookie", "Authorization", "X-Api-Key", "X-Goog-Api-Key", "X-Wackrouter":
				continue
			}
			if hop[http.CanonicalHeaderKey(name)] {
				continue
			}
			req.Header[name] = vv
		}
		setAuth(req.Header, prov, extra, rest, k.Secret)

		resp, lastErr = s.client.Do(req)
		e.Key = k.Label
		if lastErr != nil {
			s.store.NoteKey(k.ID, 0, lastErr.Error())
			resp = nil
			if r.Context().Err() != nil {
				break
			}
			continue
		}
		s.store.NoteKey(k.ID, resp.StatusCode, "")
		bad := resp.StatusCode == 429 || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode >= 500
		if bad && i < len(keys)-1 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			resp = nil
			continue
		}
		break
	}
	if resp == nil {
		msg := "upstream unreachable"
		if lastErr != nil {
			msg = lastErr.Error()
		}
		e.Status, e.Err = 502, msg
		if r.Context().Err() == context.Canceled {
			e.Status = 499
			return
		}
		oaiErr(w, 502, "upstream_error", msg)
		return
	}
	defer resp.Body.Close()

	for name, vv := range resp.Header {
		if hop[http.CanonicalHeaderKey(name)] {
			continue
		}
		w.Header()[name] = vv
	}
	w.WriteHeader(resp.StatusCode)
	e.Status = resp.StatusCode
	e.Stream = strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	cap := &capture{}
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			cap.add(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	e.In, e.Out = cap.usage()
	if resp.StatusCode >= 400 {
		e.Err = cap.errText()
	}
}

// ---- /v1/models aggregation ---------------------------------------------------------------

type modelsCache struct {
	at   time.Time
	body []byte
}

func (s *Server) invalidateModels() {
	s.mcMu.Lock()
	s.mc = nil
	s.mcMu.Unlock()
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	s.mcMu.Lock()
	if s.mc != nil && time.Since(s.mc.at) < 5*time.Minute {
		b := s.mc.body
		s.mcMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
		return
	}
	s.mcMu.Unlock()
	b := s.buildModels(r.Context())
	s.mcMu.Lock()
	s.mc = &modelsCache{time.Now(), b}
	s.mcMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) providerModels(ctx context.Context, slug string) ([]string, error) {
	prov, ok := s.store.GetProvider(slug)
	if !ok {
		return nil, io.EOF
	}
	keys, err := s.store.PickKeys(slug)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	extra := s.store.HeadersFor(prov)
	var resp *http.Response
	for _, k := range keys { // first key that answers wins
		req, _ := http.NewRequestWithContext(ctx, "GET", prov.BaseURL+"/models", nil)
		setAuth(req.Header, prov, extra, "/models", k.Secret)
		resp, err = s.client.Do(req)
		if err != nil {
			s.store.NoteKey(k.ID, 0, err.Error())
			resp = nil
			continue
		}
		s.store.NoteKey(k.ID, resp.StatusCode, "")
		if resp.StatusCode < 300 {
			break
		}
		resp.Body.Close()
		err = errors.New("upstream returned HTTP " + strconv.Itoa(resp.StatusCode))
		resp = nil
	}
	if resp == nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, strings.TrimPrefix(m.ID, "models/"))
	}
	return ids, nil
}

func (s *Server) buildModels(ctx context.Context) []byte {
	st := s.store.Snapshot()
	type item struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	list := []item{}
	for k := range st.Aliases {
		list = append(list, item{k, "model", "alias"})
	}
	for _, p := range st.Providers {
		if !p.Enabled || len(p.Keys) == 0 {
			continue
		}
		wg.Add(1)
		go func(slug string) {
			defer wg.Done()
			ids, err := s.providerModels(ctx, slug)
			if err != nil {
				return
			}
			mu.Lock()
			for _, id := range ids {
				list = append(list, item{slug + "/" + id, "model", slug})
			}
			mu.Unlock()
		}(p.ID)
	}
	wg.Wait()
	b, _ := json.Marshal(map[string]any{"object": "list", "data": list})
	return b
}
