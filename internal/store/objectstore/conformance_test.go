package objectstore

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/storetest"
)

var epoch = storetest.Epoch

// testOpts returns options on a fake clock that logs nothing.
func testOpts() (store.Options, *clock.Fake) {
	clk := clock.NewFake(epoch)
	return store.Options{Clock: clk, Logger: slog.New(slog.DiscardHandler)}, clk
}

// mustOpen opens a store on b and closes it when the test ends.
func mustOpen(t *testing.T, b Bucket, opts store.Options, options ...Option) *Store {
	t.Helper()
	s, err := Open(t.Context(), b, opts, options...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// conformanceStore opens a store for storetest. storetest calls it on a subtest's goroutine,
// so it reports to t with Errorf (goroutine-safe) and panics on an Open error, which can't
// happen on a fresh bucket short of a bug.
func conformanceStore(t *testing.T, b Bucket, o store.Options) storetest.Subject {
	s, err := Open(t.Context(), b, o)
	if err != nil {
		t.Errorf("Open: %v", err)
		panic(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestConformance runs the store suite on object stores over the fake bucket and the file://
// bucket, each on a fresh bucket.
func TestConformance(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		storetest.Run(t, func(o store.Options) storetest.Subject {
			return conformanceStore(t, NewFakeBucket(o.Clock), o)
		})
	})
	t.Run("file", func(t *testing.T) {
		storetest.Run(t, func(o store.Options) storetest.Subject {
			b, err := NewFileBucket(filepath.Join(t.TempDir(), "bucket"))
			if err != nil {
				t.Errorf("NewFileBucket: %v", err)
				panic(err)
			}
			return conformanceStore(t, b, o)
		})
	})
}
