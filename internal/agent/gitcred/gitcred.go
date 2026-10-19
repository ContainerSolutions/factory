// Package gitcred is the run pipeline's in-memory git credential helper
// (run-pipeline.md §Steps 2): git asks `factory-agent fx git-credential get` for a credential,
// and the helper asks the worker over a Unix socket that only the worker's user can open.
// The worker answers from memory, so the token is never written to disk, to .git/config or
// to a command line.
//
// The socket lives in its own directory, created 0700 for the run and removed when the
// Server closes. The worker answers only for the repos its Entries list (the task repo with
// the task token, each private read repo with its own read-only token), matched on the
// protocol, the host and the repo path (git sends the path because the pipeline sets
// credential.useHttpPath): any other repo gets no credential, so public repos are cloned
// anonymously. The entries are read at each request, so refreshed tokens apply at once.
package gitcred

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvSocket names the environment variable that carries the socket path to the helper.
const EnvSocket = "FACTORY_GIT_CREDENTIAL_SOCKET"

// maxRequest bounds a credential request or answer.
const maxRequest = 64 << 10

// ioTimeout bounds one exchange on the socket.
const ioTimeout = 10 * time.Second

// Entry is one repo the helper answers for, with its credential.
type Entry struct {
	// Protocol is the URL scheme git uses (https; http only for loopback test servers).
	Protocol string
	// Host is host[:port], as in the clone URL.
	Host string
	// Repo is owner/name.
	Repo     string
	Username string
	Password string
}

// Entries returns the current entries; the Server calls it for every request.
type Entries func() []Entry

// Server answers credential requests from memory. Close it when the run ends.
type Server struct {
	dir     string
	path    string
	ln      net.Listener
	entries Entries

	wg        sync.WaitGroup
	mu        sync.Mutex
	answered  int
	closed    bool
	closeOnce sync.Once
}

// Listen starts a Server on a socket in a new 0700 directory under parent (os.TempDir()
// when ""). Unix socket paths are short (104 bytes on macOS), so parent should be short too.
func Listen(parent string, entries Entries) (*Server, error) {
	dir, err := os.MkdirTemp(parent, "fxgc-")
	if err != nil {
		return nil, fmt.Errorf("credential socket directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("credential socket directory: %w", err)
	}
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("credential socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("credential socket: %w", err)
	}
	if entries == nil {
		entries = func() []Entry { return nil }
	}
	s := &Server{dir: dir, path: path, ln: ln, entries: entries}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Path is the socket's path (the helper's EnvSocket).
func (s *Server) Path() string { return s.path }

// Answered is how many requests got a credential.
func (s *Server) Answered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answered
}

// Close stops answering, waits for open exchanges, drops the entries and removes the socket
// directory.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		err = s.ln.Close()
		s.wg.Wait()
		s.mu.Lock()
		s.entries = nil
		s.mu.Unlock()
		if rerr := os.RemoveAll(s.dir); rerr != nil && err == nil {
			err = rerr
		}
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
	})
	return err
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(c)
		}()
	}
}

func (s *Server) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(ioTimeout))
	req, err := io.ReadAll(io.LimitReader(c, maxRequest))
	if err != nil {
		return
	}
	user, pass, ok := s.lookup(ParseRequest(req))
	if !ok {
		return
	}
	if _, err := fmt.Fprintf(c, "username=%s\npassword=%s\n", user, pass); err != nil {
		return
	}
	s.mu.Lock()
	s.answered++
	s.mu.Unlock()
}

// lookup finds the entry for a request and returns its credential.
func (s *Server) lookup(req map[string]string) (string, string, bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", "", false
	}
	entries := s.entries
	s.mu.Unlock()
	path := normaliseRepo(req["path"])
	if path == "" || entries == nil {
		return "", "", false
	}
	for _, e := range entries() {
		if !strings.EqualFold(e.Protocol, req["protocol"]) || !strings.EqualFold(e.Host, req["host"]) ||
			!strings.EqualFold(normaliseRepo(e.Repo), path) {
			continue
		}
		if e.Password == "" || !clean(e.Username) || !clean(e.Password) {
			return "", "", false
		}
		return e.Username, e.Password, true
	}
	return "", "", false
}

// clean: a value git's credential protocol can carry (no newline, no NUL).
func clean(v string) bool { return !strings.ContainsAny(v, "\n\x00") }

// normaliseRepo is a repo path without leading slashes and a trailing ".git" or slash.
func normaliseRepo(p string) string {
	p = strings.Trim(p, "/")
	return strings.TrimSuffix(p, ".git")
}

// ParseRequest parses git's credential protocol attributes (key=value lines, ending at a
// blank line or EOF). Repeated and array keys keep their last value.
func ParseRequest(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), maxRequest)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// Helper is the git credential helper (`factory-agent fx git-credential <op>`): for get it
// forwards git's request to the socket and prints the answer; store and erase are ignored
// (nothing is ever stored). Without a socket it answers nothing, and git goes on without a
// credential.
func Helper(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, socket string) error {
	if len(args) != 1 {
		return errors.New("usage: git-credential get|store|erase")
	}
	req, err := io.ReadAll(io.LimitReader(stdin, maxRequest))
	if err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	if args[0] != "get" || socket == "" {
		return nil
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	c, err := d.DialContext(dctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("the credential socket: %w", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(ioTimeout))
	if _, err := c.Write(req); err != nil {
		return fmt.Errorf("the credential socket: %w", err)
	}
	if uc, ok := c.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	if _, err := io.Copy(stdout, io.LimitReader(c, maxRequest)); err != nil {
		return fmt.Errorf("the credential socket: %w", err)
	}
	return nil
}

// HelperConfig is the git config that makes git use only this helper: an empty
// credential.helper first resets any helper from the system or global config (osxkeychain
// would store the token on disk), then the helper command, run through the shell as git
// does for "!" helpers, and credential.useHttpPath so requests carry the repo path.
func HelperConfig(helper []string) [][2]string {
	quoted := make([]string, len(helper))
	for i, a := range helper {
		quoted[i] = ShellQuote(a)
	}
	return [][2]string{
		{"credential.helper", ""},
		{"credential.helper", "!" + strings.Join(quoted, " ")},
		{"credential.useHttpPath", "true"},
	}
}

// ConfigEnv turns git config pairs into GIT_CONFIG_COUNT/KEY_n/VALUE_n environment entries:
// process-scoped config that is never written to a file.
func ConfigEnv(pairs [][2]string) []string {
	out := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(pairs))}
	for i, p := range pairs {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]))
	}
	return out
}

// ShellQuote quotes s for sh.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
