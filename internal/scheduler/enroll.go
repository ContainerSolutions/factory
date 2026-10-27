package scheduler

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// Enrollment (P1 §6): an admin mints a one-time enrollment token for a pool, an owner and a
// name; the agent host's supervisor exchanges it once for a refresh token, which re-registers
// it from then on and rotates on each use.
const (
	// KindEnrollment is an enrollment, its token hashed, named by its enl_ id.
	KindEnrollment = "Enrollment"
	// KindAgentRefresh is an enrolled agent's refresh token, hashed, named by the agent id.
	KindAgentRefresh = "AgentRefreshToken"

	// LabelEnrollmentToken and LabelRefreshToken are the hex SHA-256 of the record's token:
	// what a registration looks the record up by.
	LabelEnrollmentToken = "factory.ccf.dev/enrollment-token"
	LabelRefreshToken    = "factory.ccf.dev/refresh-token"

	// EnrollmentTokenPrefix and RefreshTokenPrefix start the two tokens (the enrollmentToken
	// and agentRefreshToken schemes).
	EnrollmentTokenPrefix = "enr_"
	RefreshTokenPrefix    = "arf_"

	// DefaultEnrollmentTTL and MaxEnrollmentTTL bound an enrollment token's life.
	DefaultEnrollmentTTL = 7 * 24 * time.Hour
	MaxEnrollmentTTL     = 30 * 24 * time.Hour
)

var (
	ownerLoginRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`)
	enrollTokRE  = regexp.MustCompile(`^enr_[A-Za-z0-9_-]{43}$`)
	refreshTokRE = regexp.MustCompile(`^arf_[A-Za-z0-9_-]{43}$`)
)

// Enrollment is the record of one enrollment. It never holds its token, only the hash.
type Enrollment struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	Spec     EnrollmentSpec      `json:"spec"`
	Status   EnrollmentStatus    `json:"status"`
}

var _ store.MetaObject = (*Enrollment)(nil)

// GetName returns metadata.name (the enl_ id).
func (e *Enrollment) GetName() string { return e.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (e *Enrollment) GetObjectMeta() *v1alpha1.ObjectMeta { return &e.Metadata }

// EnrollmentSpec is what the admin fixed.
type EnrollmentSpec struct {
	Pool string `json:"pool"`
	// Owner is the owner's login on OwnerProvider (the dispatcher's sign-in provider).
	Owner         string `json:"owner"`
	OwnerProvider string `json:"ownerProvider"`
	// Name is the agent host's name: the agent is agt_<owner>-<name>.
	Name      string    `json:"name"`
	CreatedBy string    `json:"createdBy"`
	ExpiresAt time.Time `json:"expiresAt"`
	// TokenHash is "sha256:<hex>" of the enrollment token.
	TokenHash string `json:"tokenHash"`
}

// EnrollmentStatus says whether the token was spent, and on which agent.
type EnrollmentStatus struct {
	UsedAt time.Time `json:"usedAt,omitzero"`
	Agent  string    `json:"agent,omitzero"`
}

// AgentRefresh is an enrolled agent's refresh token, stored hashed. An admin's
// deregistration deletes it.
type AgentRefresh struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	// Hash is "sha256:<hex>" of the current refresh token; each use replaces it.
	Hash string `json:"hash"`
	// Enrollment is the enrollment that registered the agent; Pool, Owner, OwnerProvider and
	// Name are its, which a registration must match.
	Enrollment    string    `json:"enrollment"`
	Pool          string    `json:"pool"`
	Owner         string    `json:"owner"`
	OwnerProvider string    `json:"ownerProvider"`
	Name          string    `json:"name"`
	RotatedAt     time.Time `json:"rotatedAt"`
}

var _ store.MetaObject = (*AgentRefresh)(nil)

// GetName returns metadata.name (the agent id).
func (a *AgentRefresh) GetName() string { return a.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (a *AgentRefresh) GetObjectMeta() *v1alpha1.ObjectMeta { return &a.Metadata }

// EnrollRequest is createEnrollment, plus who asks and the owners' provider.
type EnrollRequest struct {
	Pool, Owner, Name string
	// TTL is the token's life; 0 is DefaultEnrollmentTTL, at most MaxEnrollmentTTL.
	TTL time.Duration
	// OwnerProvider is the Provider the owner's login is on; CreatedBy the admin's actor id.
	OwnerProvider, CreatedBy string
}

func enrollmentKey(id string) store.Key { return store.Key{Kind: KindEnrollment, Name: id} }
func refreshKey(agent string) store.Key { return store.Key{Kind: KindAgentRefresh, Name: agent} }
func newEnrollment() *Enrollment        { return &Enrollment{} }
func newRefresh() *AgentRefresh         { return &AgentRefresh{} }

// tokenLabel is a token hash's label value: its hex digest.
func tokenLabel(hash string) string { return strings.TrimPrefix(hash, "sha256:") }

// newSecretToken mints prefix + 32 random bytes in base64url and returns it and its hash.
func (s *Scheduler) newSecretToken(prefix string) (token, hash string, err error) {
	b, err := s.randomBytes(tokenBytes)
	if err != nil {
		return "", "", err
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashSecret(token), nil
}

// crockford is the ULID alphabet, lower-cased.
const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

// newEnrollmentID is "enl_" and a lowercase ULID: 48 bits of milliseconds, 80 random bits.
func (s *Scheduler) newEnrollmentID() (string, error) {
	r, err := s.randomBytes(10)
	if err != nil {
		return "", err
	}
	var b [16]byte
	ms := uint64(s.clk.Now().UnixMilli()) //nolint:gosec // after 1970
	for i := range 6 {
		b[i] = byte(ms >> (40 - 8*i))
	}
	copy(b[6:], r)
	out := make([]byte, 26)
	// 128 bits as 26 base32 digits, the first carrying 3 bits.
	var acc uint64
	bits, j := 2, 0 // pad to 130 bits at the front
	for _, c := range b {
		acc = acc<<8 | uint64(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[j] = crockford[(acc>>uint(bits))&31]
			j++
		}
	}
	return "enl_" + string(out), nil
}

// CreateEnrollment is createEnrollment: it mints an enrollment for a registered pool and
// returns it and its token, which is returned only here (the record keeps its hash).
func (s *Scheduler) CreateEnrollment(ctx context.Context, req EnrollRequest) (*Enrollment, string, error) {
	ttl := req.TTL
	if ttl == 0 {
		ttl = DefaultEnrollmentTTL
	}
	switch {
	case req.Pool == "":
		return nil, "", invalid("pool", "required")
	case !ownerLoginRE.MatchString(req.Owner):
		return nil, "", invalid("owner", "%q is not a GitHub login", req.Owner)
	case !hostRE.MatchString(req.Name):
		return nil, "", invalid("name", "%q must match %s", req.Name, hostRE)
	case ttl < 0 || ttl > MaxEnrollmentTTL:
		return nil, "", invalid("ttl", "must be more than 0 and at most 30d")
	case req.OwnerProvider == "":
		return nil, "", errors.New("create enrollment: no owner provider")
	}
	cat := s.d.Catalog.Current()
	if cat == nil {
		return nil, "", errNoCatalog
	}
	if _, err := registeredPool(cat.Pools[req.Pool], req.Pool); err != nil {
		return nil, "", err
	}
	id, err := s.newEnrollmentID()
	if err != nil {
		return nil, "", err
	}
	tok, hash, err := s.newSecretToken(EnrollmentTokenPrefix)
	if err != nil {
		return nil, "", err
	}
	e := &Enrollment{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindEnrollment},
		Metadata: v1alpha1.ObjectMeta{Name: id, Labels: map[string]string{LabelEnrollmentToken: tokenLabel(hash)}},
		Spec: EnrollmentSpec{
			Pool: req.Pool, Owner: req.Owner, OwnerProvider: req.OwnerProvider, Name: req.Name,
			CreatedBy: req.CreatedBy, ExpiresAt: s.clk.Now().Add(ttl), TokenHash: hash,
		},
	}
	if err := s.d.Store.Create(ctx, e); err != nil {
		return nil, "", fmt.Errorf("store enrollment: %w", err)
	}
	s.log.InfoContext(ctx, "enrollment minted", "enrollment", id, "pool", req.Pool, "owner", req.Owner,
		"name", req.Name, "by", req.CreatedBy, "expiresAt", e.Spec.ExpiresAt)
	return e, tok, nil
}

// ListEnrollments is every enrollment, in id order.
func (s *Scheduler) ListEnrollments(ctx context.Context) ([]*Enrollment, error) {
	var out []*Enrollment
	if _, err := s.d.Store.List(ctx, KindEnrollment, store.ListOptions{}, &out); err != nil {
		return nil, fmt.Errorf("list enrollments: %w", err)
	}
	return out, nil
}

// DeleteEnrollment is deleteEnrollment: an unused enrollment's token stops working; a used
// one's record goes (its agent stays). An unknown id is ErrNotFound.
func (s *Scheduler) DeleteEnrollment(ctx context.Context, id string) error {
	err := s.d.Store.Delete(ctx, enrollmentKey(id))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("enrollment %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("delete enrollment %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "enrollment deleted", "enrollment", id)
	return nil
}

// RevokeRefresh deletes an agent's refresh token (an admin's deregistration). An agent
// without one is fine.
func (s *Scheduler) RevokeRefresh(ctx context.Context, agentID string) error {
	if err := s.d.Store.Delete(ctx, refreshKey(agentID)); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("revoke refresh token of %s: %w", agentID, err)
	}
	return nil
}

// byTokenHash finds the record of kind whose label is hash's digest and whose stored hash
// (got) is hash, compared in constant time; nil when there is none.
func byTokenHash[T store.Object](ctx context.Context, st store.Store, kind, label, hash string, got func(T) string) (T, error) {
	var zero T
	var items []T
	if _, err := st.List(ctx, kind, store.ListOptions{
		LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{label: tokenLabel(hash)}},
	}, &items); err != nil {
		return zero, fmt.Errorf("find %s: %w", kind, err)
	}
	for _, it := range items {
		if subtle.ConstantTimeCompare([]byte(got(it)), []byte(hash)) == 1 {
			return it, nil
		}
	}
	return zero, nil
}

// spendEnrollment checks an enrollment token against req's pool and host and spends it:
// unknown, spent or expired is ErrUnauthorized; another pool or name is ErrInvalid. It
// returns the enrollment, now used by agent.
func (s *Scheduler) spendEnrollment(ctx context.Context, token string, req RegisterRequest) (*Enrollment, error) {
	if !enrollTokRE.MatchString(token) {
		return nil, fmt.Errorf("malformed enrollment token: %w", ErrUnauthorized)
	}
	hash := hashSecret(token)
	e, err := byTokenHash(ctx, s.d.Store, KindEnrollment, LabelEnrollmentToken, hash, func(e *Enrollment) string { return e.Spec.TokenHash })
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("unknown enrollment token: %w", ErrUnauthorized)
	}
	if err := matchEnrolled(e.Spec.Pool, e.Spec.Name, req); err != nil {
		return nil, err
	}
	now := s.clk.Now()
	agent := AgentID(e.Spec.Owner, e.Spec.Name)
	e, err = update(ctx, s.d.Store, enrollmentKey(e.Metadata.Name), newEnrollment, func(x *Enrollment) (bool, error) {
		switch {
		case subtle.ConstantTimeCompare([]byte(x.Spec.TokenHash), []byte(hash)) != 1:
			return false, fmt.Errorf("unknown enrollment token: %w", ErrUnauthorized)
		case !x.Status.UsedAt.IsZero():
			return false, fmt.Errorf("enrollment %s is spent: %w", x.Metadata.Name, ErrUnauthorized)
		case !now.Before(x.Spec.ExpiresAt):
			return false, fmt.Errorf("enrollment %s has expired: %w", x.Metadata.Name, ErrUnauthorized)
		}
		x.Status.UsedAt, x.Status.Agent = now, agent
		return true, nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("unknown enrollment token: %w", ErrUnauthorized)
	}
	return e, err
}

// rotateRefresh checks a refresh token against req's pool and host and replaces it with a new
// one, which it returns with the record: unknown (or already rotated) is ErrUnauthorized.
func (s *Scheduler) rotateRefresh(ctx context.Context, token string, req RegisterRequest) (*AgentRefresh, string, error) {
	if !refreshTokRE.MatchString(token) {
		return nil, "", fmt.Errorf("malformed refresh token: %w", ErrUnauthorized)
	}
	hash := hashSecret(token)
	rf, err := byTokenHash(ctx, s.d.Store, KindAgentRefresh, LabelRefreshToken, hash, func(r *AgentRefresh) string { return r.Hash })
	if err != nil {
		return nil, "", err
	}
	if rf == nil {
		return nil, "", fmt.Errorf("unknown refresh token: %w", ErrUnauthorized)
	}
	if err := matchEnrolled(rf.Pool, rf.Name, req); err != nil {
		return nil, "", err
	}
	next, nextHash, err := s.newSecretToken(RefreshTokenPrefix)
	if err != nil {
		return nil, "", err
	}
	rf, err = update(ctx, s.d.Store, refreshKey(rf.Metadata.Name), newRefresh, func(x *AgentRefresh) (bool, error) {
		if subtle.ConstantTimeCompare([]byte(x.Hash), []byte(hash)) != 1 {
			return false, fmt.Errorf("the refresh token was already used: %w", ErrUnauthorized)
		}
		x.Hash, x.RotatedAt = nextHash, s.clk.Now()
		x.Metadata.Labels = map[string]string{LabelRefreshToken: tokenLabel(nextHash)}
		return true, nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", fmt.Errorf("unknown refresh token: %w", ErrUnauthorized)
	}
	if err != nil {
		return nil, "", err
	}
	return rf, next, nil
}

// putRefresh gives the agent an enrollment registered a new refresh token, replacing any it
// had, and returns the token.
func (s *Scheduler) putRefresh(ctx context.Context, agent string, e *Enrollment) (string, error) {
	tok, hash, err := s.newSecretToken(RefreshTokenPrefix)
	if err != nil {
		return "", err
	}
	rf := &AgentRefresh{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindAgentRefresh},
		Metadata: v1alpha1.ObjectMeta{Name: agent, Labels: map[string]string{LabelRefreshToken: tokenLabel(hash)}},
		Hash:     hash, Enrollment: e.Metadata.Name, Pool: e.Spec.Pool, Owner: e.Spec.Owner,
		OwnerProvider: e.Spec.OwnerProvider, Name: e.Spec.Name, RotatedAt: s.clk.Now(),
	}
	for range maxCAS {
		var cur AgentRefresh
		err := s.d.Store.Get(ctx, refreshKey(agent), &cur)
		switch {
		case errors.Is(err, store.ErrNotFound):
			rf.Metadata.ResourceVersion = ""
			if err = s.d.Store.Create(ctx, rf); errors.Is(err, store.ErrAlreadyExists) {
				continue
			}
		case err != nil:
		default:
			rf.Metadata.ResourceVersion = cur.Metadata.ResourceVersion
			if err = s.d.Store.Update(ctx, rf); errors.Is(err, store.ErrConflict) {
				continue
			}
		}
		if err != nil {
			return "", fmt.Errorf("store refresh token of %s: %w", agent, err)
		}
		return tok, nil
	}
	return "", fmt.Errorf("store refresh token of %s: %w", agent, store.ErrConflict)
}

// matchEnrolled checks that a registration names the enrollment's pool and name.
func matchEnrolled(pool, name string, req RegisterRequest) error {
	if req.Pool != pool {
		return invalid("pool", "the enrollment is for pool %q", pool)
	}
	if req.Host != name {
		return invalid("host", "the enrollment is for host %q", name)
	}
	return nil
}

// registeredPool is pool if it is a registered pool with admission (the pools enrollments
// may be minted for), else ErrInvalid.
func registeredPool(pool *v1alpha1.Pool, name string) (*v1alpha1.Pool, error) {
	if pool == nil || pool.Spec.Mode != v1alpha1.PoolRegistered || pool.Spec.Admission == nil {
		return nil, invalid("pool", "%q is not a registered pool", name)
	}
	return pool, nil
}
