// Package fakegh is a fake GitHub for factory-agent tests: the OAuth device flow
// (/login/device/code, /login/oauth/access_token) and GET /user, which the dispatcher's owner
// check calls. A user token is "tok-<login>".
package fakegh

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// UserCode is the user code the fake hands out.
const UserCode = "WDJB-MJHT"

// Server is a running fake GitHub.
type Server struct {
	*httptest.Server

	mu sync.Mutex
	// login is who approves the device login.
	login string
	// pending is how many polls answer authorization_pending before the token.
	pending int
	// slowDown makes the first poll answer slow_down.
	slowDown bool
	// deny answers access_denied.
	deny bool
	// interval is the polling interval handed out, in seconds.
	interval int
	polls    int
	codes    int
	clientID string
	scope    string
}

// New starts a fake GitHub where login approves device logins after pending polls.
func New(t *testing.T, login string, pending int) *Server {
	t.Helper()
	s := &Server{login: login, pending: pending, interval: 5}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// SetSlowDown makes the first poll answer slow_down.
func (s *Server) SetSlowDown() { s.mu.Lock(); s.slowDown = true; s.mu.Unlock() }

// SetInterval sets the polling interval handed out (default 5 s), for tests on the real clock.
func (s *Server) SetInterval(seconds int) { s.mu.Lock(); s.interval = seconds; s.mu.Unlock() }

// SetDeny makes polls answer access_denied.
func (s *Server) SetDeny() { s.mu.Lock(); s.deny = true; s.mu.Unlock() }

// Token is the user token the fake issues for its login.
func (s *Server) Token() string { return "tok-" + s.login }

// Codes is the number of device codes handed out (one per device login).
func (s *Server) Codes() int { s.mu.Lock(); defer s.mu.Unlock(); return s.codes }

// Polls is the number of token polls.
func (s *Server) Polls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.polls }

// Client is the client id and scope of the last device code request.
func (s *Server) Client() (id, scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientID, s.scope
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/login/device/code":
		_ = r.ParseForm()
		s.codes++
		s.clientID, s.scope = r.PostForm.Get("client_id"), r.PostForm.Get("scope")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev-code-1", "user_code": UserCode, "verification_uri": s.URL + "/login/device",
			"expires_in": 900, "interval": s.interval,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/login/oauth/access_token":
		_ = r.ParseForm()
		s.polls++
		if r.PostForm.Get("device_code") != "dev-code-1" || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "incorrect_device_code"})
			return
		}
		switch {
		case s.deny:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
		case s.slowDown:
			s.slowDown = false
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down", "interval": 10})
		case s.pending > 0:
			s.pending--
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": s.Token(), "token_type": "bearer"})
		}
	case r.Method == http.MethodGet && r.URL.Path == "/user":
		login, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		if !ok || login == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"login": login})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}
