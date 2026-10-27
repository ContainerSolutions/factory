// Package enroll keeps an enrolled agent's registration credential (P1 §6): the refresh token
// in the supervisor's state dir, 0600, and before the first registration the one-time
// enrollment token from the file the config names. Tokens are never logged, and errors never
// carry them.
package enroll

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ContainerSolutions/factory/internal/agent/config"
)

// RefreshFile is the refresh token's file in the state dir.
const RefreshFile = "agent-refresh-token"

// Files is the enrollment credential on disk. It implements worker.Enrollment.
type Files struct {
	// StateDir holds RefreshFile (supervise's --state-dir, absolute).
	StateDir string
	// TokenFile is the config's enrollmentTokenFile: read only while StateDir has no refresh
	// token. "" when the config sets none.
	TokenFile string
}

func (f Files) refreshPath() string { return filepath.Join(f.StateDir, RefreshFile) }

// Enrolled reports whether the state dir holds a refresh token.
func (f Files) Enrolled() bool {
	_, err := os.Lstat(f.refreshPath())
	return err == nil
}

// ErrNoCredential is Token's error when there is neither a refresh token nor an enrollment
// token file.
var ErrNoCredential = errors.New("no refresh token in the state dir and no enrollmentTokenFile: mint an enrollment with `factory agent enroll` and set enrollmentTokenFile to its token's file")

// Token returns the refresh token when the state dir has one (refresh true), else the
// enrollment token from TokenFile. Both files must be 0600 single lines.
func (f Files) Token() (string, bool, error) {
	tok, err := config.ReadSecretFile(f.refreshPath())
	switch {
	case err == nil:
		return tok, true, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", false, fmt.Errorf("the refresh token: %w", err)
	case f.TokenFile == "":
		return "", false, ErrNoCredential
	}
	tok, err = config.ReadSecretFile(f.TokenFile)
	if err != nil {
		return "", false, fmt.Errorf("enrollmentTokenFile: %w", err)
	}
	return tok, false, nil
}

// Save writes the next refresh token to the state dir, 0600, atomically (a temp file renamed
// over the old one), before the registration that returned it is used.
func (f Files) Save(refresh string) error {
	if err := os.MkdirAll(f.StateDir, 0o700); err != nil {
		return fmt.Errorf("save the refresh token: %w", err)
	}
	p := f.refreshPath()
	tmp, err := os.CreateTemp(f.StateDir, "."+RefreshFile+".tmp-")
	if err != nil {
		return fmt.Errorf("save the refresh token: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	_, werr := tmp.WriteString(refresh + "\n")
	if werr == nil {
		werr = tmp.Chmod(0o600)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, p)
	}
	if werr != nil {
		return fmt.Errorf("save the refresh token: %w", werr)
	}
	if d, err := os.Open(f.StateDir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Forget removes the refresh token (the dispatcher refused it), so the next start reads
// TokenFile.
func (f Files) Forget() error {
	if err := os.Remove(f.refreshPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the refused refresh token: %w", err)
	}
	return nil
}
