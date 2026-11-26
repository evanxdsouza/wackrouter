package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- data model ----------------------------------------------------------

type Key struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Secret  string `json:"secret"` // AES-GCM encrypted, base64
	Hint    string `json:"hint"`   // last 4 chars, for display only
	Enabled bool   `json:"enabled"`
	Added   int64  `json:"added"`
}

// A custom header sent to the provider with every request (value is encrypted at rest).
type Header struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
	Hint   string `json:"hint"`
}

type HeaderIn struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Provider struct {
	ID         string   `json:"id"` // slug, used as the model prefix: "openai/gpt-4o"
	Name       string   `json:"name"`
	Kind       string   `json:"kind"` // "openai" (Bearer) | "anthropic" (x-api-key) | "custom" (AuthHeader)
	BaseURL    string   `json:"base_url"`
	Enabled    bool     `json:"enabled"`
	AuthHeader string   `json:"auth_header,omitempty"` // kind=custom: header that carries the key, e.g. api-key
	AuthPrefix string   `json:"auth_prefix,omitempty"` // kind=custom: text before the key, e.g. "Bearer "
	Headers    []Header `json:"headers,omitempty"`
	Models     []string `json:"models,omitempty"` // allowlist; empty means every model the provider offers
	Keys       []Key    `json:"keys"`
}

type Token struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hash     string `json:"hash"`
	Hint     string `json:"hint"`
	Enabled  bool   `json:"enabled"`
	Created  int64  `json:"created"`
	LastUsed int64  `json:"last_used"`
	Requests int64  `json:"requests"`
}

type Data struct {
	Providers       []Provider        `json:"providers"`
	Tokens          []Token           `json:"tokens"`
	Aliases         map[string]string `json:"aliases"`
	DefaultProvider string            `json:"default_provider"`
	Allow           []string          `json:"allow,omitempty"` // global model allowlist (canonical names); empty = every model
	Sinks           []Sink            `json:"sinks"`
	PassHash        string            `json:"pass_hash,omitempty"`
	PassSalt        string            `json:"pass_salt,omitempty"`
}

type KeyStatus struct {
	At     int64
	Status int
	Err    string
}

type Store struct {
	mu     sync.RWMutex
	path   string
	master []byte
	gcm    cipher.AEAD
	D      Data
	rr     map[string]uint64
	ks     map[string]KeyStatus
	ss     map[string]SinkStatus
	dirty  bool
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// OpenStore loads (or creates) the data dir. The second return value is a
// freshly generated admin password, non-empty only on first run.
func OpenStore(dir string) (*Store, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	s := &Store{path: filepath.Join(dir, "wackrouter.json"), rr: map[string]uint64{}, ks: map[string]KeyStatus{}, ss: map[string]SinkStatus{}}

	secret := os.Getenv("WACKROUTER_SECRET")
	if secret == "" {
		p := filepath.Join(dir, "master.key")
		b, err := os.ReadFile(p)
		if err != nil {
			secret = randHex(32)
			if err := os.WriteFile(p, []byte(secret), 0o600); err != nil {
				return nil, "", err
			}
		} else {
			secret = strings.TrimSpace(string(b))
		}
	}
	sum := sha256.Sum256([]byte(secret))
	s.master = sum[:]
	blk, err := aes.NewCipher(s.master)
	if err != nil {
		return nil, "", err
	}
	if s.gcm, err = cipher.NewGCM(blk); err != nil {
		return nil, "", err
	}

	if b, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(b, &s.D); err != nil {
			return nil, "", fmt.Errorf("corrupt %s: %w", s.path, err)
		}
	}
	if s.D.Aliases == nil {
		s.D.Aliases = map[string]string{}
	}

	gen := ""
	if os.Getenv("WACKROUTER_PASSWORD") == "" && s.D.PassHash == "" {
		gen = randHex(8)
		s.setPassLocked(gen)
		if err := s.save(); err != nil {
			return nil, "", err
		}
	}
	return s, gen, nil
}

func (s *Store) save() error { // caller holds write lock (or is single-threaded)
	b, err := json.MarshalIndent(s.D, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	s.dirty = false
	return os.Rename(tmp, s.path)
}

// Flush persists lazily-updated counters (token usage).
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.save()
	}
}

// ---- crypto ----------------------------------------------------------------

func (s *Store) enc(plain string) string {
	nonce := make([]byte, s.gcm.NonceSize())
	rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(s.gcm.Seal(nonce, nonce, []byte(plain), nil))
}

func (s *Store) dec(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) < s.gcm.NonceSize() {
		return "", errors.New("bad ciphertext")
	}
	n := s.gcm.NonceSize()
	out, err := s.gcm.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt (wrong WACKROUTER_SECRET / master.key?)")
	}
	return string(out), nil
}

// ---- admin password & sessions ----------------------------------------------

func hashPass(pw string, salt []byte) string {
	k, _ := pbkdf2.Key(sha256.New, pw, salt, 210000, 32)
	return hex.EncodeToString(k)
}

func (s *Store) setPassLocked(pw string) {
	salt := make([]byte, 16)
	rand.Read(salt)
	s.D.PassSalt = hex.EncodeToString(salt)
	s.D.PassHash = hashPass(pw, salt)
}

func (s *Store) SetPassword(pw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setPassLocked(pw)
	return s.save()
}

func (s *Store) CheckPassword(pw string) bool {
	if e := os.Getenv("WACKROUTER_PASSWORD"); e != "" {
		return subtle.ConstantTimeCompare([]byte(e), []byte(pw)) == 1
	}
	s.mu.RLock()
	salt, _ := hex.DecodeString(s.D.PassSalt)
	want := s.D.PassHash
	s.mu.RUnlock()
	return subtle.ConstantTimeCompare([]byte(hashPass(pw, salt)), []byte(want)) == 1
}

func (s *Store) sessionMAC(msg string) string {
	s.mu.RLock()
	salt := s.D.PassSalt
	s.mu.RUnlock()
	m := hmac.New(sha256.New, s.master)
	m.Write([]byte("sess:" + salt + ":" + msg))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Store) NewSession(ttl time.Duration) string {
	exp := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	return exp + "." + s.sessionMAC(exp)
}

func (s *Store) ValidSession(v string) bool {
	i := strings.IndexByte(v, '.')
	if i < 0 {
		return false
	}
	exp, err := strconv.ParseInt(v[:i], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(v[i+1:]), []byte(s.sessionMAC(v[:i])))
}

// ---- providers & keys -------------------------------------------------------

func (s *Store) idx(pid string) int {
	for i := range s.D.Providers {
		if s.D.Providers[i].ID == pid {
			return i
		}
	}
	return -1
}

func (s *Store) GetProvider(pid string) (Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i := s.idx(pid); i >= 0 {
		return s.D.Providers[i], true
	}
	return Provider{}, false
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

var reservedHeaders = map[string]bool{"host": true, "content-length": true, "transfer-encoding": true, "connection": true}

// UpsertProvider creates or updates a provider. hdrs == nil keeps the stored custom
// headers; a non-nil slice replaces them (an empty value keeps that header's stored value).
func (s *Store) UpsertProvider(p Provider, hdrs *[]HeaderIn, models *[]string) error {
	if !slugRe.MatchString(p.ID) {
		return errors.New("id must be lowercase letters, digits or dashes (max 32)")
	}
	if p.Kind != "openai" && p.Kind != "anthropic" && p.Kind != "custom" {
		return errors.New("kind must be openai, anthropic or custom")
	}
	p.AuthHeader, p.AuthPrefix = strings.TrimSpace(p.AuthHeader), strings.TrimLeft(p.AuthPrefix, "\t\r\n")
	if p.Kind == "custom" {
		if !headerNameRe.MatchString(p.AuthHeader) {
			return errors.New("custom auth needs a header name, like api-key or X-Auth-Token")
		}
	} else {
		p.AuthHeader, p.AuthPrefix = "", ""
	}
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
		return errors.New("base URL must start with http:// or https://")
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = p.ID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.idx(p.ID)
	var old []Header
	if i >= 0 {
		old = s.D.Providers[i].Headers
	}
	p.Headers = old
	if i >= 0 {
		p.Models = s.D.Providers[i].Models
	}
	if models != nil {
		p.Models = nil
		seen := map[string]bool{}
		for _, m := range *models {
			if m = strings.TrimSpace(m); m != "" && !seen[m] {
				seen[m] = true
				p.Models = append(p.Models, m)
			}
		}
	}
	if hdrs != nil {
		p.Headers = nil
		seen := map[string]bool{}
		for _, h := range *hdrs {
			name := strings.TrimSpace(h.Name)
			if name == "" {
				continue
			}
			if !headerNameRe.MatchString(name) || reservedHeaders[strings.ToLower(name)] {
				return errors.New("can't use \"" + name + "\" as a custom header name")
			}
			if seen[strings.ToLower(name)] {
				return errors.New("header \"" + name + "\" is listed twice")
			}
			seen[strings.ToLower(name)] = true
			if h.Value != "" {
				p.Headers = append(p.Headers, Header{Name: name, Secret: s.enc(h.Value), Hint: last4(h.Value)})
				continue
			}
			kept := false
			for _, o := range old {
				if strings.EqualFold(o.Name, name) {
					o.Name = name
					p.Headers = append(p.Headers, o)
					kept = true
				}
			}
			if !kept {
				return errors.New("header \"" + name + "\" needs a value")
			}
		}
	}
	if i >= 0 {
		p.Keys = s.D.Providers[i].Keys
		s.D.Providers[i] = p
	} else {
		p.Keys = nil
		s.D.Providers = append(s.D.Providers, p)
	}
	return s.save()
}

// HeadersFor returns the provider's custom headers, decrypted.
func (s *Store) HeadersFor(p Provider) map[string]string {
	out := map[string]string{}
	for _, h := range p.Headers {
		if v, err := s.dec(h.Secret); err == nil {
			out[h.Name] = v
		}
	}
	return out
}

func (s *Store) DeleteProvider(pid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.idx(pid)
	if i < 0 {
		return errors.New("not found")
	}
	s.D.Providers = append(s.D.Providers[:i], s.D.Providers[i+1:]...)
	if s.D.DefaultProvider == pid {
		s.D.DefaultProvider = ""
	}
	return s.save()
}

func (s *Store) AddKey(pid, label, secret string) error {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return errors.New("key is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.idx(pid)
	if i < 0 {
		return errors.New("unknown provider")
	}
	hint := secret
	if len(hint) > 4 {
		hint = hint[len(hint)-4:]
	}
	if strings.TrimSpace(label) == "" {
		label = "key " + strconv.Itoa(len(s.D.Providers[i].Keys)+1)
	}
	s.D.Providers[i].Keys = append(s.D.Providers[i].Keys, Key{
		ID: randHex(4), Label: label, Secret: s.enc(secret), Hint: hint, Enabled: true, Added: time.Now().Unix(),
	})
	return s.save()
}

func (s *Store) SetKeyEnabled(pid, kid string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.idx(pid); i >= 0 {
		for j := range s.D.Providers[i].Keys {
			if s.D.Providers[i].Keys[j].ID == kid {
				s.D.Providers[i].Keys[j].Enabled = on
				return s.save()
			}
		}
	}
	return errors.New("not found")
}

func (s *Store) DeleteKey(pid, kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.idx(pid); i >= 0 {
		ks := s.D.Providers[i].Keys
		for j := range ks {
			if ks[j].ID == kid {
				s.D.Providers[i].Keys = append(ks[:j:j], ks[j+1:]...)
				return s.save()
			}
		}
	}
	return errors.New("not found")
}

type Picked struct{ ID, Label, Secret string }

// PickKeys returns the provider's enabled keys decrypted, rotated round-robin
// so successive calls start on a different key. The tail is the failover order.
func (s *Store) PickKeys(pid string) ([]Picked, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.idx(pid)
	if i < 0 {
		return nil, errors.New("unknown provider")
	}
	var en []Key
	for _, k := range s.D.Providers[i].Keys {
		if k.Enabled {
			en = append(en, k)
		}
	}
	if len(en) == 0 {
		return nil, errors.New("provider has no enabled keys")
	}
	start := int(s.rr[pid] % uint64(len(en)))
	s.rr[pid]++
	var out []Picked
	var lastErr error
	for n := range en {
		k := en[(start+n)%len(en)]
		sec, err := s.dec(k.Secret)
		if err != nil {
			lastErr = err
			continue
		}
		out = append(out, Picked{k.ID, k.Label, sec})
	}
	if len(out) == 0 {
		return nil, lastErr
	}
	return out, nil
}

func (s *Store) NoteKey(kid string, status int, errMsg string) {
	s.mu.Lock()
	s.ks[kid] = KeyStatus{At: time.Now().Unix(), Status: status, Err: errMsg}
	s.mu.Unlock()
}

// DefaultSlug is the provider used when a model has no recognised prefix.
func (s *Store) DefaultSlug() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.idx(s.D.DefaultProvider) >= 0 {
		return s.D.DefaultProvider
	}
	for _, p := range s.D.Providers {
		if p.Enabled && len(p.Keys) > 0 {
			return p.ID
		}
	}
	return ""
}

func (s *Store) Resolve(model string) (slug, m string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.D.Aliases[model]; ok {
		model = t
	}
	if i := strings.IndexByte(model, '/'); i > 0 && s.idx(model[:i]) >= 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

func (s *Store) SetAliases(a map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clean := map[string]string{}
	for k, v := range a {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			clean[k] = v
		}
	}
	s.D.Aliases = clean
	return s.save()
}

// AllowSet returns the global allowlist as a set of canonical names, or nil when everything is allowed.
func (s *Store) AllowSet() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.D.Allow) == 0 {
		return nil
	}
	m := map[string]bool{}
	for _, a := range s.D.Allow {
		m[canon(a)] = true
	}
	return m
}

func (s *Store) SetAllow(a []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, v := range a {
		k := canon(v)
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	s.D.Allow = out
	return s.save()
}

func (s *Store) SetDefault(pid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pid != "" && s.idx(pid) < 0 {
		return errors.New("unknown provider")
	}
	s.D.DefaultProvider = pid
	return s.save()
}

// ---- client tokens ----------------------------------------------------------

func (s *Store) CreateToken(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name required")
	}
	secret := "wr-" + randHex(24)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.D.Tokens = append(s.D.Tokens, Token{
		ID: randHex(4), Name: name, Hash: sha256hex(secret),
		Hint: secret[:7] + "…" + secret[len(secret)-4:], Enabled: true, Created: time.Now().Unix(),
	})
	return secret, s.save()
}

func (s *Store) SetTokenEnabled(id string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.D.Tokens {
		if s.D.Tokens[i].ID == id {
			s.D.Tokens[i].Enabled = on
			return s.save()
		}
	}
	return errors.New("not found")
}

func (s *Store) DeleteToken(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.D.Tokens {
		if s.D.Tokens[i].ID == id {
			s.D.Tokens = append(s.D.Tokens[:i], s.D.Tokens[i+1:]...)
			return s.save()
		}
	}
	return errors.New("not found")
}

func (s *Store) AuthToken(secret string) (string, bool) {
	if secret == "" {
		return "", false
	}
	h := sha256hex(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.D.Tokens {
		t := &s.D.Tokens[i]
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 {
			if !t.Enabled {
				return "", false
			}
			t.LastUsed = time.Now().Unix()
			t.Requests++
			s.dirty = true
			return t.Name, true
		}
	}
	return "", false
}

func (s *Store) HasTokens() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.D.Tokens) > 0
}

// ---- snapshot for the UI (never includes secrets) ---------------------------

type apiKey struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Hint       string `json:"hint"`
	Enabled    bool   `json:"enabled"`
	Added      int64  `json:"added"`
	LastAt     int64  `json:"last_at"`
	LastStatus int    `json:"last_status"`
	LastErr    string `json:"last_err"`
}
type apiHeader struct {
	Name string `json:"name"`
	Hint string `json:"hint"`
}
type apiProvider struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Kind       string      `json:"kind"`
	BaseURL    string      `json:"base_url"`
	Enabled    bool        `json:"enabled"`
	AuthHeader string      `json:"auth_header"`
	AuthPrefix string      `json:"auth_prefix"`
	Headers    []apiHeader `json:"headers"`
	Models     []string    `json:"models"`
	Keys       []apiKey    `json:"keys"`
}
type apiToken struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hint     string `json:"hint"`
	Enabled  bool   `json:"enabled"`
	Created  int64  `json:"created"`
	LastUsed int64  `json:"last_used"`
	Requests int64  `json:"requests"`
}
type State struct {
	Providers []apiProvider     `json:"providers"`
	Tokens    []apiToken        `json:"tokens"`
	Aliases   map[string]string `json:"aliases"`
	Default   string            `json:"default_provider"`
	Allow     []string          `json:"allow"`
	Sinks     []apiSink         `json:"sinks"`
	EnvPass   bool              `json:"env_password"`
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := State{Providers: []apiProvider{}, Tokens: []apiToken{}, Aliases: map[string]string{}, Default: s.D.DefaultProvider, Allow: append([]string{}, s.D.Allow...), EnvPass: os.Getenv("WACKROUTER_PASSWORD") != "", Sinks: s.sinkSnapshot()}
	for k, v := range s.D.Aliases {
		st.Aliases[k] = v
	}
	for _, p := range s.D.Providers {
		ap := apiProvider{ID: p.ID, Name: p.Name, Kind: p.Kind, BaseURL: p.BaseURL, Enabled: p.Enabled, AuthHeader: p.AuthHeader, AuthPrefix: p.AuthPrefix, Headers: []apiHeader{}, Models: append([]string{}, p.Models...), Keys: []apiKey{}}
		for _, h := range p.Headers {
			ap.Headers = append(ap.Headers, apiHeader{h.Name, h.Hint})
		}
		for _, k := range p.Keys {
			x := s.ks[k.ID]
			ap.Keys = append(ap.Keys, apiKey{k.ID, k.Label, k.Hint, k.Enabled, k.Added, x.At, x.Status, x.Err})
		}
		st.Providers = append(st.Providers, ap)
	}
	for _, t := range s.D.Tokens {
		st.Tokens = append(st.Tokens, apiToken{t.ID, t.Name, t.Hint, t.Enabled, t.Created, t.LastUsed, t.Requests})
	}
	return st
}

// AllowedModels returns the provider's model allowlist as a set, or nil when every model is allowed.
func (s *Store) AllowedModels(slug string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := s.idx(slug)
	if i < 0 || len(s.D.Providers[i].Models) == 0 {
		return nil
	}
	m := make(map[string]bool, len(s.D.Providers[i].Models))
	for _, id := range s.D.Providers[i].Models {
		m[id] = true
	}
	return m
}
