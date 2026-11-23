package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	store  *Store
	logs   *Logs
	stats  *Stats
	cat    []catItem
	catAt  time.Time
	meta   map[string]ModelMeta
	ship   *Shipper
	client *http.Client
	mcMu   sync.Mutex
	mc     *modelsCache
	failMu sync.Mutex
	fails  map[string]*failInfo
}

type failInfo struct {
	n     int
	until time.Time
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	dir := env("WACKROUTER_DATA", "./data")
	addr := env("WACKROUTER_ADDR", ":8787")
	if p := os.Getenv("PORT"); p != "" && os.Getenv("WACKROUTER_ADDR") == "" {
		addr = ":" + p
	}
	st, gen, err := OpenStore(dir)
	if err != nil {
		log.Fatal(err)
	}
	s := &Server{
		store: st,
		logs:  OpenLogs(filepath.Join(dir, "requests.jsonl")),
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			TLSHandshakeTimeout:   10 * time.Second,
		}},
		fails: map[string]*failInfo{},
		meta:  map[string]ModelMeta{},
	}
	s.ship = NewShipper(st, s.client)
	s.stats = OpenStats(filepath.Join(dir, "stats.json"), s.logs.Recent(logCap), s.costOf)
	s.logs.hook = func(e LogEntry) {
		s.ship.Enqueue(e)
		s.stats.Add(e, s.costOf(e))
	}
	go func() { // warm the model catalog so prices are known before the first request lands
		time.Sleep(3 * time.Second)
		s.catalog(context.Background())
	}()
	go func() {
		for range time.Tick(30 * time.Second) {
			st.Flush()
			s.stats.Flush()
		}
	}()

	log.Printf("wackrouter listening on %s (data: %s)", addr, dir)
	if gen != "" {
		log.Printf("admin password (shown once, change it in Settings): %s", gen)
	}
	srv := &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/", s.handleV1)
	mux.HandleFunc("/p/", s.handlePassthrough)

	mux.HandleFunc("POST /api/login", s.apiLogin)
	mux.HandleFunc("POST /api/logout", s.apiLogout)
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]bool{"ok": s.loggedIn(r)})
	})
	a := s.admin
	mux.HandleFunc("GET /api/state", a(func(w http.ResponseWriter, r *http.Request) { jsonOut(w, s.store.Snapshot()) }))
	mux.HandleFunc("PUT /api/providers/{id}", a(s.apiPutProvider))
	mux.HandleFunc("DELETE /api/providers/{id}", a(s.apiDelProvider))
	mux.HandleFunc("POST /api/providers/{id}/keys", a(s.apiAddKey))
	mux.HandleFunc("PATCH /api/providers/{id}/keys/{kid}", a(s.apiPatchKey))
	mux.HandleFunc("DELETE /api/providers/{id}/keys/{kid}", a(s.apiDelKey))
	mux.HandleFunc("POST /api/providers/{id}/test", a(s.apiTest))
	mux.HandleFunc("PUT /api/sinks/{id}", a(s.apiPutSink))
	mux.HandleFunc("PATCH /api/sinks/{id}", a(s.apiPatchSink))
	mux.HandleFunc("DELETE /api/sinks/{id}", a(s.apiDelSink))
	mux.HandleFunc("POST /api/sinks/{id}/test", a(s.apiTestSink))
	mux.HandleFunc("POST /api/tokens", a(s.apiNewToken))
	mux.HandleFunc("PATCH /api/tokens/{id}", a(s.apiPatchToken))
	mux.HandleFunc("DELETE /api/tokens/{id}", a(s.apiDelToken))
	mux.HandleFunc("PUT /api/aliases", a(s.apiAliases))
	mux.HandleFunc("PUT /api/default", a(s.apiDefault))
	mux.HandleFunc("GET /api/logs", a(func(w http.ResponseWriter, r *http.Request) { jsonOut(w, s.logs.Recent(1000)) }))
	mux.HandleFunc("DELETE /api/logs", a(func(w http.ResponseWriter, r *http.Request) { s.logs.Clear(); jsonOut(w, ok{}) }))
	mux.HandleFunc("GET /api/models", a(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.buildModels(r.Context()))
	}))
	mux.HandleFunc("GET /api/model-stats", a(s.apiModelStats))
	mux.HandleFunc("GET /api/usage", a(s.apiUsage))
	mux.HandleFunc("POST /api/chat", a(s.apiChat))
	mux.HandleFunc("POST /api/password", a(s.apiPassword))

	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	return mux
}

// ---- admin plumbing -----------------------------------------------------------------

type ok struct {
	OK bool `json:"ok"`
}

func jsonOut(w http.ResponseWriter, v any) {
	if _, isOK := v.(ok); isOK {
		v = map[string]bool{"ok": true}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie("wr_session")
	return err == nil && s.store.ValidSession(c.Value)
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.loggedIn(r) {
			jsonErr(w, 401, "login required")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Wackrouter") == "" {
			jsonErr(w, 403, "missing X-Wackrouter header")
			return
		}
		h(w, r)
	}
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	s.failMu.Lock()
	f := s.fails[ip]
	if f != nil && time.Now().Before(f.until) {
		s.failMu.Unlock()
		jsonErr(w, 429, "too many attempts, wait a minute")
		return
	}
	s.failMu.Unlock()

	var in struct {
		Password string `json:"password"`
	}
	if readJSON(r, &in) != nil || !s.store.CheckPassword(in.Password) {
		s.failMu.Lock()
		if s.fails[ip] == nil {
			s.fails[ip] = &failInfo{}
		}
		s.fails[ip].n++
		if s.fails[ip].n >= 5 {
			s.fails[ip].until = time.Now().Add(time.Minute)
			s.fails[ip].n = 0
		}
		s.failMu.Unlock()
		jsonErr(w, 401, "wrong password")
		return
	}
	s.failMu.Lock()
	delete(s.fails, ip)
	s.failMu.Unlock()
	ttl := 7 * 24 * time.Hour
	http.SetCookie(w, &http.Cookie{
		Name: "wr_session", Value: s.store.NewSession(ttl), Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
	jsonOut(w, ok{})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "wr_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	jsonOut(w, ok{})
}

// ---- admin handlers --------------------------------------------------------------------

func (s *Server) apiPutProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string      `json:"name"`
		Kind       string      `json:"kind"`
		BaseURL    string      `json:"base_url"`
		Enabled    bool        `json:"enabled"`
		AuthHeader string      `json:"auth_header"`
		AuthPrefix string      `json:"auth_prefix"`
		Headers    *[]HeaderIn `json:"headers"`
		Models     *[]string   `json:"models"`
		Key        string      `json:"key"`
		KeyLabel   string      `json:"key_label"`
	}
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	id := r.PathValue("id")
	p := Provider{ID: id, Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL, Enabled: in.Enabled, AuthHeader: in.AuthHeader, AuthPrefix: in.AuthPrefix}
	if err := s.store.UpsertProvider(p, in.Headers, in.Models); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(in.Key) != "" {
		if err := s.store.AddKey(id, in.KeyLabel, in.Key); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
	}
	s.invalidateModels()
	jsonOut(w, ok{})
}

func (s *Server) apiDelProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteProvider(r.PathValue("id")); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	s.invalidateModels()
	jsonOut(w, ok{})
}

func (s *Server) apiAddKey(w http.ResponseWriter, r *http.Request) {
	var in struct{ Label, Key string }
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	if err := s.store.AddKey(r.PathValue("id"), in.Label, in.Key); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	s.invalidateModels()
	jsonOut(w, ok{})
}

func (s *Server) apiPatchKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	if err := s.store.SetKeyEnabled(r.PathValue("id"), r.PathValue("kid"), in.Enabled); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	s.invalidateModels()
	jsonOut(w, ok{})
}

func (s *Server) apiDelKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteKey(r.PathValue("id"), r.PathValue("kid")); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	s.invalidateModels()
	jsonOut(w, ok{})
}

func (s *Server) apiTest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ids, err := s.providerModels(r.Context(), r.PathValue("id"))
	res := map[string]any{"ok": err == nil, "ms": time.Since(start).Milliseconds()}
	if err != nil {
		res["error"] = err.Error()
	} else {
		res["models"] = len(ids)
	}
	jsonOut(w, res)
}

func (s *Server) apiNewToken(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	secret, err := s.store.CreateToken(in.Name)
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	jsonOut(w, map[string]string{"secret": secret})
}

func (s *Server) apiPatchToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	if err := s.store.SetTokenEnabled(r.PathValue("id"), in.Enabled); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	jsonOut(w, ok{})
}

func (s *Server) apiDelToken(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteToken(r.PathValue("id")); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	jsonOut(w, ok{})
}

func (s *Server) apiAliases(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	if err := s.store.SetAliases(in); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	s.invalidateModels()
	jsonOut(w, ok{})
}

func (s *Server) apiDefault(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
	}
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	if err := s.store.SetDefault(in.Provider); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	jsonOut(w, ok{})
}

func (s *Server) apiPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Current, New string }
	if readJSON(r, &in) != nil || len(in.New) < 8 {
		jsonErr(w, 400, "new password must be at least 8 characters")
		return
	}
	if os.Getenv("WACKROUTER_PASSWORD") != "" {
		jsonErr(w, 400, "password is set by WACKROUTER_PASSWORD; change the environment variable instead")
		return
	}
	if !s.store.CheckPassword(in.Current) {
		jsonErr(w, 401, "current password is wrong")
		return
	}
	s.store.SetPassword(in.New)
	http.SetCookie(w, &http.Cookie{Name: "wr_session", Value: s.store.NewSession(7 * 24 * time.Hour), Path: "/", MaxAge: 7 * 86400, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	jsonOut(w, ok{})
}

// Playground: runs through the same routing/forwarding path as external clients.
func (s *Server) apiChat(w http.ResponseWriter, r *http.Request) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/v1/chat/completions"
	r2.URL.RawQuery = ""
	r2.Header = http.Header{"Content-Type": {"application/json"}}
	s.serve(w, r2, "playground")
}

// ---- drains -------------------------------------------------------------------------

func (s *Server) apiPutSink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		URL     string `json:"url"`
		Source  string `json:"source"`
		Key     string `json:"key"`
		Enabled bool   `json:"enabled"`
	}
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	k := Sink{ID: r.PathValue("id"), Name: in.Name, Type: in.Type, URL: in.URL, Source: in.Source, Enabled: in.Enabled}
	if err := s.store.UpsertSink(k, in.Key); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	jsonOut(w, ok{})
}

func (s *Server) apiPatchSink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if readJSON(r, &in) != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	if err := s.store.SetSinkEnabled(r.PathValue("id"), in.Enabled); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	jsonOut(w, ok{})
}

func (s *Server) apiDelSink(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSink(r.PathValue("id")); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	jsonOut(w, ok{})
}

func (s *Server) apiTestSink(w http.ResponseWriter, r *http.Request) {
	ms, err := s.ship.Test(r.PathValue("id"))
	res := map[string]any{"ok": err == nil, "ms": ms}
	if err != nil {
		res["error"] = err.Error()
	}
	jsonOut(w, res)
}
