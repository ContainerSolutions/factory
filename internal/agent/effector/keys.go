package effector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// maxKey is the dispatcher's bound on an idempotency key; maxUserKey bounds a --key.
const (
	maxKey     = 200
	maxUserKey = 100
)

// keyStore derives idempotency keys (effector.md §Behaviour 2, refined in AGT-S1-T05):
//
//   - with a skill-chosen --key: <task>/<op>/<key>, the same effect however often it's sent;
//   - else <task>/<op>/<h>.<n>, where h is a hash of the op, target and arguments and n is
//     how many calls with that same hash already succeeded in this run.
//
// So a call that failed or never got its answer (a network error, a crash, a 429) and is
// sent again gets the same key, and the dispatcher replays it instead of applying it twice;
// once a call succeeded, the same command again is a new effect. The counts are kept in the
// state file ($FACTORY_OUT/.effector-seq), locked for every update, so they hold across CLI
// processes, the MCP server and adapter restarts.
type keyStore struct {
	path string

	mu  sync.Mutex
	mem map[string]int // when path is ""
}

// keyState is the state file.
type keyState struct {
	// Done counts the successful calls per "<op>/<hash>".
	Done map[string]int `json:"done"`
}

// reserve returns the key for one call and a commit to run once the dispatcher accepted
// it.
func (k *keyStore) reserve(task string, op v1alpha1.EffectOp, target string, args map[string]any, key string) (string, func() error, error) {
	if key != "" {
		if len(key) > maxUserKey || strings.ContainsAny(key, " \t\r\n") {
			return "", nil, usagef("--key must be 1 to %d characters without spaces", maxUserKey)
		}
		full := task + "/" + string(op) + "/" + key
		if len(full) > maxKey {
			return "", nil, usagef("the idempotency key %q is over %d characters", full, maxKey)
		}
		return full, func() error { return nil }, nil
	}
	b, err := json.Marshal(map[string]any{"op": op, "target": target, "args": args})
	if err != nil {
		return "", nil, fmt.Errorf("hash the arguments: %w", err)
	}
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:8])
	id := string(op) + "/" + h
	var n int
	if err := k.update(func(s *keyState) { n = s.Done[id] }); err != nil {
		return "", nil, err
	}
	full := fmt.Sprintf("%s/%s/%s.%d", task, op, h, n)
	if len(full) > maxKey {
		return "", nil, usagef("the task id is too long for an idempotency key")
	}
	commit := func() error {
		return k.update(func(s *keyState) { s.Done[id] = max(s.Done[id], n+1) })
	}
	return full, commit, nil
}

// update applies f to the state under the lock and saves it.
func (k *keyStore) update(f func(*keyState)) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.path == "" {
		if k.mem == nil {
			k.mem = map[string]int{}
		}
		s := keyState{Done: k.mem}
		f(&s)
		return nil
	}
	file, err := os.OpenFile(k.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("the effector state: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := lockFile(file); err != nil {
		return fmt.Errorf("lock the effector state: %w", err)
	}
	defer func() { _ = unlockFile(file) }()
	b, err := io.ReadAll(io.LimitReader(file, 4<<20))
	if err != nil {
		return fmt.Errorf("read the effector state: %w", err)
	}
	var s keyState
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &s); err != nil {
			return errors.Join(fmt.Errorf("the effector state %s is corrupt", k.path), err)
		}
	}
	if s.Done == nil {
		s.Done = map[string]int{}
	}
	f(&s)
	out, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode the effector state: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("write the effector state: %w", err)
	}
	if _, err := file.WriteAt(append(out, '\n'), 0); err != nil {
		return fmt.Errorf("write the effector state: %w", err)
	}
	return nil
}
