package objectstore

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
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
	if got := blobRuns(r); !slices.Equal(got, []string{"r1", store.EnvironmentLogBlob("r1")}) {
		t.Fatalf("blobs after the re-put = %v: the replaced blob should be gone", got)
	}
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

// TestTranscriptOrphans: the daily scan deletes a blob whose id no record names once it is a
// day old (a crash between the blob and its record, or a replaced blob whose delete failed),
// and leaves the blob a record names, a young orphan, and names it doesn't recognise.
func TestTranscriptOrphans(t *testing.T) {
	r := newRig()
	s := r.open(t, "A")
	ctx := t.Context()
	must(t, s.PutTranscript(ctx, "kept", []byte("k")))
	kept := r.b.Names(transcriptPrefix)[0]
	r.b.SetObject(blobName("orphan", "00000000000000aa"), []byte("o")) // its record never written
	r.b.SetObject(blobName("kept", "00000000000000bb"), []byte("old")) // a replaced blob left behind
	r.b.SetObject(transcriptPrefix+"stray.jsonl.gz", []byte("s"))      // not a name the store writes
	r.clk.Advance(23 * time.Hour)
	r.b.SetObject(blobName("young", "00000000000000cc"), []byte("y"))
	r.clk.Advance(time.Hour)
	_, err := s.Sweep(ctx)
	must(t, err)
	want := []string{kept, blobName("young", "00000000000000cc"), transcriptPrefix + "stray.jsonl.gz"}
	slices.Sort(want)
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
		if got := blobRuns(r); !slices.Equal(got, []string{"x", "z"}) {
			t.Fatalf("blobs of %v, want one each of x and z", got)
		}
		var recs []TranscriptBlob
		_, err := a.List(ctx, KindTranscriptBlob, store.ListOptions{}, &recs)
		must(t, err)
		if len(recs) != 2 || recs[0].Metadata.Name != "x" || recs[0].Size != 10 {
			t.Fatalf("records = %+v, want x (re-put) and z", recs)
		}
	})
	t.Run("eviction, re-put after the record delete", testRePutAfterRecordDelete)
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

// blobRuns returns the run of every blob in the bucket, sorted.
func blobRuns(r *rig) []string {
	var runs []string
	for _, n := range r.b.Names(transcriptPrefix) {
		run, _, ok := blobOf(n)
		if !ok {
			run = "?" + n
		}
		runs = append(runs, run)
	}
	slices.Sort(runs)
	return runs
}

// testRePutAfterRecordDelete (TestTranscriptDropRacesRePut; ccf-review on #238): A's eviction of x commits the
// delete of x's record; before A deletes x's blob, B re-puts x. B's put creates a blob of its
// own and a new record, so A's blob delete can't touch it: x stays readable on both stores, is
// counted, and only A's old blob goes.
func testRePutAfterRecordDelete(t *testing.T) {
	r := newRig()
	r.opts.TranscriptMaxBytes, r.opts.TranscriptTotalBytes = 10, 20
	a, b := r.open(t, "A"), r.open(t, "B")
	ctx := t.Context()
	must(t, a.PutTranscript(ctx, "x", bytes.Repeat([]byte("1"), 10)))
	r.clk.Advance(time.Second)
	must(t, a.PutTranscript(ctx, "y", bytes.Repeat([]byte("2"), 10)))
	if _, err := b.GetTranscript(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(time.Second)
	var rePut error
	raced := false
	r.b.SetFaults(func(req FakeRequest) Fault {
		if req.Op == FakeDelete && strings.HasPrefix(req.Name, transcriptPrefix+"x.") && !raced {
			raced = true
			rePut = b.PutTranscript(ctx, "x", bytes.Repeat([]byte("3"), 10))
		}
		return Pass
	})
	must(t, a.PutTranscript(ctx, "z", bytes.Repeat([]byte("4"), 10))) // evicts x
	r.b.SetFaults(nil)
	must(t, rePut)
	if !raced {
		t.Fatal("A never deleted x's blob")
	}
	for _, s := range []*Store{a, b} {
		must(t, s.Sync(ctx))
		if got, err := s.GetTranscript(ctx, "x"); err != nil || string(got) != strings.Repeat("3", 10) {
			t.Fatalf("%s reads x = %q, %v; want B's re-put", s.id, got, err)
		}
		var rec TranscriptBlob
		if err := s.Get(ctx, store.Key{Kind: KindTranscriptBlob, Name: "x"}, &rec); err != nil || rec.Size != 10 {
			t.Fatalf("%s: x's record = %+v, %v; want it counted", s.id, rec, err)
		}
	}
	if got := blobRuns(r); slices.Index(got, "x") < 0 {
		t.Fatalf("blobs of %v: x's re-put blob is gone", got)
	}
}

// TestTranscriptGetFollowsRePut: A holds x's old record; between A reading it and A reading its
// blob, B re-puts x (a new blob, the record moved to it, the old blob deleted). A's read finds
// the old blob gone, syncs once and follows the record to the new blob, with no Sync of the
// caller's. A read-only A answers from what it has.
func TestTranscriptGetFollowsRePut(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("readOnly=%v", readOnly), func(t *testing.T) {
			r := newRig()
			a, b := r.open(t, "A"), r.open(t, "B")
			ctx := t.Context()
			must(t, a.PutTranscript(ctx, "x", []byte("old")))
			if _, err := b.GetTranscript(ctx, "x"); err != nil {
				t.Fatal(err)
			}
			old := r.b.Names(transcriptPrefix)[0]
			raced := false
			r.b.SetFaults(func(req FakeRequest) Fault {
				if req.Op != FakeGet || req.Name != old || raced {
					return Pass
				}
				raced = true
				r.b.SetFaults(nil)
				defer r.b.SetFaults(func(FakeRequest) Fault { return Pass })
				must(t, b.PutTranscript(ctx, "x", []byte("new")))
				if readOnly { // then a newer release writes an entry A can't read
					c := r.open(t, "C", withSchema(store.SchemaVersion+2, store.SchemaVersion+2))
					must(t, c.Create(ctx, newRun("newer")))
				}
				return Pass
			})
			got, err := a.GetTranscript(ctx, "x")
			if !raced {
				t.Fatal("A never read the old blob")
			}
			must(t, err)
			if string(got) != "new" {
				t.Fatalf("A reads %q, want B's re-put", got)
			}
			if readOnly {
				if err := a.Ready(); !errors.Is(err, store.ErrReadOnly) {
					t.Fatalf("A Ready = %v, want read-only", err)
				}
			}
		})
	}
}

// TestTranscriptEvictionBlobDeleteFails: an eviction whose record delete commits but whose
// blob delete fails still counts as evicted (the record is gone): the put goes through, a
// warning says the blob waits for the orphan scan, and the scan deletes it a day later.
func TestTranscriptEvictionBlobDeleteFails(t *testing.T) {
	r := newRig()
	r.opts.TranscriptMaxBytes, r.opts.TranscriptTotalBytes = 10, 10
	var logs bytes.Buffer
	r.opts.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := r.open(t, "A")
	ctx := t.Context()
	must(t, s.PutTranscript(ctx, "x", make([]byte, 10)))
	r.b.SetFaults(func(req FakeRequest) Fault {
		if req.Op == FakeDelete && strings.HasPrefix(req.Name, transcriptPrefix) {
			return Fail
		}
		return Pass
	})
	must(t, s.PutTranscript(ctx, "y", make([]byte, 10)))
	r.b.SetFaults(nil)
	if _, err := s.GetTranscript(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("x after its eviction = %v, want ErrNotFound", err)
	}
	if !strings.Contains(logs.String(), "orphan scan") {
		t.Fatalf("no warning about the blob left behind: %q", logs.String())
	}
	if got := blobRuns(r); !slices.Equal(got, []string{"x", "y"}) {
		t.Fatalf("blobs = %v, want x's left behind and y", got)
	}
	r.clk.Advance(orphanAge)
	_, err := s.Sweep(ctx)
	must(t, err)
	if got := blobRuns(r); !slices.Equal(got, []string{"y"}) {
		t.Fatalf("blobs after the orphan scan = %v, want y", got)
	}
}
