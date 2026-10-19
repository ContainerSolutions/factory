// Package githubtest is an httptest GitHub for the GitHub adapter's tests. It serves the App
// endpoints the token minter uses (GET /app, GET /repos/{owner}/{repo}/installation and
// POST /app/installations/{id}/access_tokens), verifies every App JWT against the App's public
// key, records each request for assertions and can be made to fail.
//
// It is safe for concurrent use. Getters return copies.
package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/clock"
)

// TokenTTL is the lifetime of the tokens the server mints, as on GitHub.
const TokenTTL = time.Hour

// Installation is one App installation on an account.
type Installation struct {
	ID    int64
	AppID int64
	// Owner is the account the App is installed on ("compliance-framework").
	Owner string
	// Repos are the repo names (without owner) the installation covers; empty means all.
	Repos []string
	// Permissions are what the installation was granted.
	Permissions map[string]string
}

// Claims are the claims of a verified App JWT.
type Claims struct {
	Issuer    string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Request is one request the server received, with the App JWT it carried.
type Request struct {
	Method string
	Path   string
	// Body is the raw request body.
	Body []byte
	// Claims are the JWT's claims; zero when the request carried no valid JWT.
	Claims Claims
}

// TokenRequest is one access-token request, decoded.
type TokenRequest struct {
	InstallationID int64
	// Body is the raw request body, for exact assertions.
	Body         []byte
	Repositories []string
	// HasRepositories is false when the body had no "repositories" field at all.
	HasRepositories bool
	Permissions     map[string]string
}

// Failure is a canned response for the next request on a route.
type Failure struct {
	Status  int
	Message string
	Header  http.Header
}

// Routes, for FailNext and Count.
const (
	RouteApp          = "app"
	RouteInstallation = "installation"
	RouteAccessToken  = "access_tokens"
)

// Server is an httptest GitHub.
type Server struct {
	*httptest.Server

	clock clock.Clock

	mu            sync.Mutex
	apps          map[int64]*rsa.PublicKey
	installations []Installation
	requests      []Request
	tokenRequests []TokenRequest
	counts        map[string]int
	failures      map[string][]Failure
	seq           int
	gate          chan struct{}
	inFlight      int
}

// NewServer starts a server stamping token expiry with c; it is closed with the test.
func NewServer(t testing.TB, c clock.Clock) *Server {
	t.Helper()
	s := &Server{
		clock:    c,
		apps:     map[int64]*rsa.PublicKey{},
		counts:   map[string]int{},
		failures: map[string][]Failure{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app", s.handleApp)
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", s.handleInstallation)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", s.handleAccessToken)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// AddApp registers an App and its public key.
func (s *Server) AddApp(appID int64, key *rsa.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apps[appID] = key
}

// AddInstallation installs an App on an account.
func (s *Server) AddInstallation(in Installation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in.Repos = slices.Clone(in.Repos)
	in.Permissions = maps.Clone(in.Permissions)
	s.installations = append(s.installations, in)
}

// SetInstallationPermissions changes what an installation was granted.
func (s *Server) SetInstallationPermissions(id int64, perms map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.installations {
		if s.installations[i].ID == id {
			s.installations[i].Permissions = maps.Clone(perms)
		}
	}
}

// FailNext makes the next request on route answer f instead.
func (s *Server) FailNext(route string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[route] = append(s.failures[route], f)
}

// Gate holds every access-token request until the returned release func is called, so a test
// can pile concurrent callers onto one request.
func (s *Server) Gate() (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := make(chan struct{})
	s.gate = g
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.gate = nil
			s.mu.Unlock()
			close(g)
		})
	}
}

// InFlight is the number of access-token requests held by the gate.
func (s *Server) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

// Count is the number of requests received on route, failed ones included.
func (s *Server) Count(route string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[route]
}

// Requests returns every request received, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	for i, r := range s.requests {
		r.Body = slices.Clone(r.Body)
		out[i] = r
	}
	return out
}

// TokenRequests returns the access-token requests received, in order.
func (s *Server) TokenRequests() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TokenRequest, len(s.tokenRequests))
	for i, r := range s.tokenRequests {
		r.Body = slices.Clone(r.Body)
		r.Repositories = slices.Clone(r.Repositories)
		r.Permissions = maps.Clone(r.Permissions)
		out[i] = r
	}
	return out
}

// Tokens returns every token value the server minted, in order.
func (s *Server) Tokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, s.seq)
	for i := range s.seq {
		out[i] = tokenValue(i + 1)
	}
	return out
}

func tokenValue(n int) string { return fmt.Sprintf("ghs_testtoken%06d", n) }

// begin records the request, verifies its JWT and returns the App it authenticates, or writes
// the failure response and returns ok == false.
func (s *Server) begin(w http.ResponseWriter, r *http.Request, route string) (appID int64, body []byte, ok bool) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable body")
		return 0, nil, false
	}
	s.mu.Lock()
	s.counts[route]++
	claims, appID, jwtErr := s.verifyLocked(r.Header.Get("Authorization"))
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Body: body, Claims: claims})
	var fail *Failure
	if q := s.failures[route]; len(q) > 0 {
		fail = &q[0]
		s.failures[route] = q[1:]
	}
	s.mu.Unlock()

	if fail != nil {
		for k, vs := range fail.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		writeError(w, fail.Status, fail.Message)
		return 0, nil, false
	}
	if jwtErr != nil {
		writeError(w, http.StatusUnauthorized, "A JSON web token could not be decoded: "+jwtErr.Error())
		return 0, nil, false
	}
	return appID, body, true
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	appID, _, ok := s.begin(w, r, RouteApp)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": appID, "slug": fmt.Sprintf("app-%d", appID)})
}

func (s *Server) handleInstallation(w http.ResponseWriter, r *http.Request) {
	appID, _, ok := s.begin(w, r, RouteInstallation)
	if !ok {
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	s.mu.Lock()
	in, found := s.findLocked(func(in Installation) bool {
		return in.AppID == appID && strings.EqualFold(in.Owner, owner) && covers(in, repo)
	})
	s.mu.Unlock()
	if !found {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                   in.ID,
		"account":              map[string]any{"login": in.Owner},
		"app_id":               in.AppID,
		"permissions":          in.Permissions,
		"repository_selection": selection(in),
	})
}

func (s *Server) handleAccessToken(w http.ResponseWriter, r *http.Request) {
	appID, body, ok := s.begin(w, r, RouteAccessToken)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	var raw map[string]json.RawMessage
	var req struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	_, hasRepos := raw["repositories"]

	s.mu.Lock()
	s.tokenRequests = append(s.tokenRequests, TokenRequest{
		InstallationID: id, Body: body, Repositories: req.Repositories, HasRepositories: hasRepos, Permissions: req.Permissions,
	})
	in, found := s.findLocked(func(in Installation) bool { return in.ID == id && in.AppID == appID })
	gate := s.gate
	if gate != nil {
		s.inFlight++
	}
	s.mu.Unlock()
	if gate != nil {
		<-gate
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}

	if !found {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, name := range req.Repositories {
		if !covers(in, name) {
			writeError(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
			return
		}
	}
	for perm, level := range req.Permissions {
		if rank(level) == 0 || rank(in.Permissions[perm]) < rank(level) {
			writeError(w, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
			return
		}
	}
	perms := req.Permissions
	if perms == nil {
		perms = in.Permissions
	}
	repos := make([]map[string]any, 0, len(req.Repositories))
	for _, name := range req.Repositories {
		repos = append(repos, map[string]any{"name": name, "full_name": in.Owner + "/" + name})
	}

	s.mu.Lock()
	s.seq++
	tok := tokenValue(s.seq)
	s.mu.Unlock()
	resp := map[string]any{
		"token":                tok,
		"expires_at":           s.clock.Now().Add(TokenTTL).UTC().Format(time.RFC3339),
		"permissions":          perms,
		"repository_selection": "selected",
	}
	if len(repos) > 0 {
		resp["repositories"] = repos
	} else {
		resp["repository_selection"] = selection(in)
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) findLocked(match func(Installation) bool) (Installation, bool) {
	for _, in := range s.installations {
		if match(in) {
			return in, true
		}
	}
	return Installation{}, false
}

func covers(in Installation, repo string) bool {
	return len(in.Repos) == 0 || slices.ContainsFunc(in.Repos, func(r string) bool { return strings.EqualFold(r, repo) })
}

func selection(in Installation) string {
	if len(in.Repos) == 0 {
		return "all"
	}
	return "selected"
}

func rank(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// verifyLocked checks an "Authorization: Bearer <jwt>" header: RS256, signed by a known App,
// issued for it, not expired and valid for at most 10 minutes, as GitHub requires. JWT times
// are checked against the wall clock: the App transport stamps them with it.
func (s *Server) verifyLocked(header string) (Claims, int64, error) {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return Claims{}, 0, fmt.Errorf("no bearer token")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, 0, fmt.Errorf("not a JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil || hdr.Alg != "RS256" {
		return Claims{}, 0, fmt.Errorf("bad header")
	}
	var c struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return Claims{}, 0, fmt.Errorf("bad claims")
	}
	appID, err := strconv.ParseInt(c.Iss, 10, 64)
	if err != nil {
		return Claims{}, 0, fmt.Errorf("bad issuer")
	}
	key, ok := s.apps[appID]
	if !ok {
		return Claims{}, 0, fmt.Errorf("unknown app %d", appID)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, 0, fmt.Errorf("bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, 0, fmt.Errorf("bad signature")
	}
	claims := Claims{Issuer: c.Iss, IssuedAt: time.Unix(c.Iat, 0), ExpiresAt: time.Unix(c.Exp, 0)}
	now := time.Now()
	switch {
	case !claims.ExpiresAt.After(now):
		return claims, 0, fmt.Errorf("expired")
	case claims.IssuedAt.After(now.Add(time.Minute)):
		return claims, 0, fmt.Errorf("issued in the future")
	case claims.ExpiresAt.Sub(claims.IssuedAt) > 10*time.Minute:
		return claims, 0, fmt.Errorf("valid for more than 10 minutes")
	}
	return claims, appID, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("X-GitHub-Request-Id", "TEST:REQ")
	writeJSON(w, status, map[string]string{"message": msg, "documentation_url": "https://docs.github.com/rest"})
}

var (
	keyOnce sync.Once
	keyPEM  []byte
	keyRSA  *rsa.PrivateKey
	errKey  error
)

// AppKey returns a throwaway RSA key and its PKCS#1 PEM encoding (GitHub's format), generated
// once per test binary.
func AppKey(t testing.TB) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	keyOnce.Do(func() {
		keyRSA, errKey = rsa.GenerateKey(rand.Reader, 2048)
		if errKey == nil {
			keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(keyRSA)})
		}
	})
	if errKey != nil {
		t.Fatalf("generate app key: %v", errKey)
	}
	return slices.Clone(keyPEM), keyRSA
}

// NewAppKey returns a fresh throwaway RSA key and its PEM encoding, for a second App.
func NewAppKey(t testing.TB) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate app key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), k
}
