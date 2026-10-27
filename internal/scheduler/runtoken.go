package scheduler

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// RunTokenPrefix starts every run token (the runToken scheme's format). It differs from the
// run IDs' run_ so a token is always recognisable as a credential.
const RunTokenPrefix = "rtk_"

// runTokenRE is the runToken scheme's format: the prefix and 32 random bytes in base64url.
var runTokenRE = regexp.MustCompile(`^rtk_[A-Za-z0-9_-]{43}$`)

// newRunToken mints a lease's run token (P1 §5) and returns it and its stored hash.
func (s *Scheduler) newRunToken() (token, hash string, err error) {
	b, err := s.randomBytes(tokenBytes)
	if err != nil {
		return "", "", err
	}
	token = RunTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashSecret(token), nil
}

// runTokenLabel is a run token hash's LabelRunToken value: its hex digest.
func runTokenLabel(hash string) string { return strings.TrimPrefix(hash, "sha256:") }

// AuthenticateRun checks a run token and returns what it authenticates. The token's lease
// must be live: not ended (completed, abandoned, expired or revoked) and not past its
// expiry. Every refusal wraps ErrUnauthorized and never carries the token.
func (s *Scheduler) AuthenticateRun(ctx context.Context, token string) (RunAuth, error) {
	if !runTokenRE.MatchString(token) {
		return RunAuth{}, fmt.Errorf("malformed run token: %w", ErrUnauthorized)
	}
	hash := hashSecret(token)
	var leases []*Lease
	if _, err := s.d.Store.List(ctx, KindLease, store.ListOptions{
		LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{LabelRunToken: runTokenLabel(hash)}},
	}, &leases); err != nil {
		return RunAuth{}, fmt.Errorf("find the run token's lease: %w", err)
	}
	var l *Lease
	for _, x := range leases {
		if subtle.ConstantTimeCompare([]byte(x.Spec.RunTokenHash), []byte(hash)) == 1 {
			l = x
			break
		}
	}
	switch {
	case l == nil:
		return RunAuth{}, fmt.Errorf("unknown run token: %w", ErrUnauthorized)
	case l.Status.End != "":
		return RunAuth{}, fmt.Errorf("the run token's lease has ended (%s): %w", l.Status.End, ErrUnauthorized)
	case !s.clk.Now().Before(l.Status.ExpiresAt):
		return RunAuth{}, fmt.Errorf("the run token's lease has expired: %w", ErrUnauthorized)
	}
	return RunAuth{Lease: l.Metadata.Name, Task: l.Spec.Task, Agent: l.Spec.Agent}, nil
}
