package cel

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestCacheConcurrent compiles and evaluates the same and different expressions from many
// goroutines (run with -race): each source compiles to one shared Program, and evaluations
// don't interfere.
func TestCacheConcurrent(t *testing.T) {
	env := newTestEnv(t)
	srcs := []string{
		"item.count('implement') < 3",
		"glob('compliance-framework/*', event.repo)",
		"item.state.stackNext + 1 < item.state.stackTotal",
		"item.pr.approvedBy.exists(a, a.identity == 'reviewer' && a.commit == item.pr.head)",
	}
	const goroutines, rounds = 16, 50
	got := make([][]*Program, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := pocActivation()
			for r := range rounds {
				// A source unique to this goroutine and round, and the shared ones.
				p, err := env.Compile(fmt.Sprintf("hash(item.id) %% %d >= 0", g*rounds+r+1), Where)
				if err != nil {
					t.Error(err)
					return
				}
				if ok, err := p.EvalBool(context.Background(), a); err != nil || !ok {
					t.Errorf("unique: %v %v", ok, err)
				}
				for _, src := range srcs {
					p, err := env.Compile(src, Where)
					if err != nil {
						t.Error(err)
						return
					}
					if r == 0 {
						got[g] = append(got[g], p)
					}
					if ok, err := p.EvalBool(context.Background(), a); err != nil || !ok {
						t.Errorf("%s: %v %v", src, ok, err)
					}
				}
			}
		}()
	}
	wg.Wait()
	for g := 1; g < goroutines; g++ {
		for i := range srcs {
			if got[g][i] != got[0][i] {
				t.Errorf("goroutine %d compiled %q to a different Program", g, srcs[i])
			}
		}
	}
	env.mu.RLock()
	n := len(env.cache)
	env.mu.RUnlock()
	if want := len(srcs) + goroutines*rounds; n != want {
		t.Errorf("cache holds %d programs, want %d", n, want)
	}
	// Kind is part of the key.
	w, _ := env.Compile("true", Where)
	v, _ := env.Compile("true", Value)
	if w == v || w.Kind() != Where || v.Kind() != Value {
		t.Error("where and value share a cache entry")
	}
}
