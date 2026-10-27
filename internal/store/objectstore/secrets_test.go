package objectstore_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/journal"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
	"github.com/ContainerSolutions/factory/internal/store/storeaudit"
	"github.com/ContainerSolutions/factory/internal/store/storetest"
)

// stored returns the log entries and snapshots (gunzipped) in the bucket: the bytes the store
// writes about objects (transcript blobs are agent output, not store objects).
func stored(t *testing.T, b *objectstore.FakeBucket) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for name, data := range b.Objects() {
		switch {
		case strings.HasPrefix(name, "log/"):
			out[name] = data
		case strings.HasPrefix(name, "snapshots/"):
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			plain, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out[name] = plain
		}
	}
	return out
}

// kindsIn returns the kinds of the objects in the log entries and snapshots.
func kindsIn(t *testing.T, files map[string][]byte) map[string]bool {
	t.Helper()
	kinds := map[string]bool{}
	for name, data := range files {
		if strings.HasPrefix(name, "snapshots/") {
			var snap struct {
				Objects []journal.SnapshotObject `json:"objects"`
			}
			if err := json.Unmarshal(data, &snap); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, o := range snap.Objects {
				kinds[o.Kind] = true
			}
			continue
		}
		line, _, err := journal.DecodeLine(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		kinds[line.Kind] = true
	}
	return kinds
}

// audit checks that every kind in the bucket has an audit line and that no token-shaped string
// is anywhere in its log entries and snapshots.
func audit(t *testing.T, files map[string][]byte) {
	t.Helper()
	if len(files) == 0 {
		t.Fatal("nothing stored")
	}
	for k := range kindsIn(t, files) {
		if storeaudit.KindAudit[k] == "" {
			t.Errorf("kind %s reaches the bucket with no line in storeaudit.KindAudit: check that its stored form holds no credential and add a line saying why", k)
		}
	}
	for name, data := range files {
		if hits := storeaudit.FindTokens(data); len(hits) > 0 {
			t.Errorf("%s holds token-shaped strings: %v", name, hits)
		}
	}
}

func open(t *testing.T, b objectstore.Bucket, o store.Options) *objectstore.Store {
	t.Helper()
	var options []objectstore.Option
	for kind, proto := range storeaudit.InternalKinds() {
		options = append(options, objectstore.WithKind(kind, proto))
	}
	s, err := objectstore.Open(t.Context(), b, o, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSecretsFixture writes a real-shaped object of every audited kind through the object
// store (locks and transcripts make its own), compacts, and audits the log and the snapshot.
func TestSecretsFixture(t *testing.T) {
	clk := clock.NewFake(storeaudit.Epoch)
	o := store.Options{Clock: clk, Logger: slog.New(slog.DiscardHandler)}
	b := objectstore.NewFakeBucket(clk)
	s := open(t, b, o)
	ctx := t.Context()
	for _, obj := range storeaudit.Fixtures() {
		if err := s.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}
	u, err := s.Lock(ctx, "wi/wi-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Renew(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	u.Unlock()
	if _, err := s.Lock(ctx, "held", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTranscript(ctx, "run_1", []byte("\x1f\x8bgzip-bytes")); err != nil {
		t.Fatal(err)
	}
	var d = storeaudit.Delivery("gh-1")
	if err := s.Get(ctx, store.KeyOf(d), d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Body, d.Spec.Headers = nil, nil
	if err := s.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "run_gone"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	files := stored(t, b)
	kinds := kindsIn(t, files)
	for k := range storeaudit.KindAudit {
		if !kinds[k] {
			t.Errorf("audited kind %s never reached the bucket: add a real-shaped one to storeaudit.Fixtures", k)
		}
	}
	audit(t, files)
}

// TestSecretsConformance audits what the conformance suite stored, on every store it opened.
func TestSecretsConformance(t *testing.T) {
	var (
		mu      sync.Mutex
		buckets []*objectstore.FakeBucket
	)
	t.Run("suite", func(t *testing.T) {
		storetest.Run(t, func(o store.Options) storetest.Subject {
			b := objectstore.NewFakeBucket(o.Clock)
			mu.Lock()
			buckets = append(buckets, b)
			mu.Unlock()
			s, err := objectstore.Open(t.Context(), b, o)
			if err != nil {
				t.Errorf("Open: %v", err)
				panic(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		})
	})
	all := map[string][]byte{}
	for i, b := range buckets {
		for name, data := range stored(t, b) {
			all[fmt.Sprintf("%s (store %d)", name, i)] = data
		}
	}
	audit(t, all)
}
