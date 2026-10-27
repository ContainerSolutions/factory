package objectstore

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/store"
)

// TestTranscriptsAcrossStores: a blob put on A is read on B; replacing it on B updates its
// record; deleting the Run on A removes the blobs and records of the transcript and the
// environment log.
func TestTranscriptsAcrossStores(t *testing.T) {
	r := newRig()
	a, b := r.open(t, "A"), r.open(t, "B")
	ctx := t.Context()
	must(t, a.Create(ctx, newRun("r1")))
	must(t, a.PutTranscript(ctx, "r1", []byte("v1")))
	must(t, a.PutTranscript(ctx, store.EnvironmentLogBlob("r1"), []byte("env")))
	if got, err := b.GetTranscript(ctx, "r1"); err != nil || string(got) != "v1" {
		t.Fatalf("B reads %q, %v", got, err)
	}
	r.clk.Advance(time.Minute)
	must(t, b.PutTranscript(ctx, "r1", []byte("v2!")))
	var rec TranscriptBlob
	must(t, a.Get(ctx, store.Key{Kind: KindTranscriptBlob, Name: "r1"}, &rec))
	must(t, a.Sync(ctx))
	must(t, a.Get(ctx, store.Key{Kind: KindTranscriptBlob, Name: "r1"}, &rec))
	if rec.Size != 3 || !rec.PutAt.Equal(epoch.Add(time.Minute)) {
		t.Fatalf("record after B's replace = %+v", rec)
	}
	if got, err := a.GetTranscript(ctx, "r1"); err != nil || string(got) != "v2!" {
		t.Fatalf("A reads %q, %v", got, err)
	}

	must(t, a.Delete(ctx, runKey("r1")))
	if got := r.b.Names(transcriptPrefix); len(got) != 0 {
		t.Fatalf("blobs after the Run's delete: %v", got)
	}
	for _, s := range []*Store{a, b} {
		for _, run := range []string{"r1", store.EnvironmentLogBlob("r1")} {
			if _, err := s.GetTranscript(ctx, run); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s GetTranscript(%s) after the Run's delete = %v", s.id, run, err)
			}
		}
	}
}

// TestTranscriptOrphans: the daily scan deletes a blob that has had no record for a day (a
// crash between the put and the record), and leaves a blob with a record and a young orphan.
func TestTranscriptOrphans(t *testing.T) {
	r := newRig()
	s := r.open(t, "A")
	ctx := t.Context()
	must(t, s.PutTranscript(ctx, "kept", []byte("k")))
	r.b.SetObject(transcriptPrefix+"orphan.jsonl.gz", []byte("o")) // its record never written
	r.clk.Advance(23 * time.Hour)
	r.b.SetObject(transcriptPrefix+"young.jsonl.gz", []byte("y"))
	r.clk.Advance(time.Hour)
	_, err := s.Sweep(ctx)
	must(t, err)
	want := []string{transcriptPrefix + "kept.jsonl.gz", transcriptPrefix + "young.jsonl.gz"}
	if got := r.b.Names(transcriptPrefix); !slices.Equal(got, want) {
		t.Fatalf("blobs after the scan = %v, want %v", got, want)
	}
	// Once a day: the next sweep doesn't list again.
	lists := r.b.Calls(FakeList)
	_, err = s.Sweep(ctx)
	must(t, err)
	if r.b.Calls(FakeList) != lists {
		t.Fatal("the orphan scan ran twice in a day")
	}
}

// TestTranscriptEvictionOrder: eviction goes by putAt, and on a tie by resourceVersion: a
// replaced blob counts as new.
func TestTranscriptEvictionOrder(t *testing.T) {
	r := newRig()
	r.opts.TranscriptMaxBytes, r.opts.TranscriptTotalBytes = 10, 20
	s := r.open(t, "A")
	ctx := t.Context()
	for _, run := range []string{"a", "b"} { // same putAt: a is older by version
		must(t, s.PutTranscript(ctx, run, make([]byte, 10)))
	}
	must(t, s.PutTranscript(ctx, "c", make([]byte, 10)))
	if _, err := s.GetTranscript(ctx, "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a (oldest by version on a putAt tie) = %v, want evicted", err)
	}
	r.clk.Advance(time.Second)
	must(t, s.PutTranscript(ctx, "b", make([]byte, 10))) // b is now the newest
	r.clk.Advance(time.Second)
	must(t, s.PutTranscript(ctx, "d", make([]byte, 10)))
	for run, want := range map[string]bool{"b": true, "c": false, "d": true} {
		if _, err := s.GetTranscript(ctx, run); (err == nil) != want {
			t.Fatalf("transcript %s present=%v, want %v", run, err == nil, want)
		}
	}
}

// TestTranscriptDropRacesRePut: a store that evicts or sweeps a transcript while another store
// re-puts it loses the record's conditional delete (ErrConflict) and leaves the new blob: it
// stays readable on both stores and counted, and the eviction moves on to the next oldest.
func TestTranscriptDropRacesRePut(t *testing.T) {
	t.Run("eviction", func(t *testing.T) {
		r := newRig()
		r.opts.TranscriptMaxBytes, r.opts.TranscriptTotalBytes = 10, 20
		rc := &racer{}
		r.b.SetFaults(rc.hook)
		a, b := r.open(t, "A"), r.open(t, "B")
		ctx := t.Context()
		must(t, a.PutTranscript(ctx, "x", bytes.Repeat([]byte("1"), 10)))
		r.clk.Advance(time.Second)
		must(t, a.PutTranscript(ctx, "y", bytes.Repeat([]byte("2"), 10)))
		if _, err := b.GetTranscript(ctx, "x"); err != nil { // B syncs both
			t.Fatal(err)
		}
		r.clk.Advance(time.Second)
		var rePut error
		rc.before(func() { rePut = b.PutTranscript(ctx, "x", bytes.Repeat([]byte("3"), 10)) })
		must(t, a.PutTranscript(ctx, "z", bytes.Repeat([]byte("4"), 10))) // evicts x, which B re-puts meanwhile
		must(t, rePut)
		for _, s := range []*Store{a, b} {
			if got, err := s.GetTranscript(ctx, "x"); err != nil || string(got) != strings.Repeat("3", 10) {
				t.Fatalf("%s reads x = %q, %v; want B's re-put", s.id, got, err)
			}
			if _, err := s.GetTranscript(ctx, "y"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s reads y = %v, want it evicted in x's place", s.id, err)
			}
		}
		if got := r.b.Names(transcriptPrefix); !slices.Equal(got, []string{transcriptPrefix + "x.jsonl.gz", transcriptPrefix + "z.jsonl.gz"}) {
			t.Fatalf("blobs = %v", got)
		}
		var recs []TranscriptBlob
		_, err := a.List(ctx, KindTranscriptBlob, store.ListOptions{}, &recs)
		must(t, err)
		if len(recs) != 2 || recs[0].Metadata.Name != "x" || recs[0].Size != 10 {
			t.Fatalf("records = %+v, want x (re-put) and z", recs)
		}
	})
	t.Run("sweep", func(t *testing.T) {
		r := newRig()
		rc := &racer{}
		r.b.SetFaults(rc.hook)
		a, b := r.open(t, "A"), r.open(t, "B")
		ctx := t.Context()
		must(t, a.PutTranscript(ctx, "x", []byte("old")))
		if _, err := b.GetTranscript(ctx, "x"); err != nil {
			t.Fatal(err)
		}
		r.clk.Advance(store.DefaultRetention)
		var rePut error
		rc.before(func() { rePut = b.PutTranscript(ctx, "x", []byte("new")) })
		res, err := a.Sweep(ctx)
		must(t, err)
		must(t, rePut)
		if res.Transcripts != 0 {
			t.Fatalf("swept %d transcripts, want 0: x was re-put", res.Transcripts)
		}
		for _, s := range []*Store{a, b} {
			if got, err := s.GetTranscript(ctx, "x"); err != nil || string(got) != "new" {
				t.Fatalf("%s reads x = %q, %v; want the re-put", s.id, got, err)
			}
		}
	})
}
