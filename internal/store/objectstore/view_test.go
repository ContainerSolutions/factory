package objectstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/memory"
)

// TestView: Read loads the newest snapshot and the log above it, lists the entries above the
// snapshot, follows a transcript's record to its blob, and writes nothing.
func TestView(t *testing.T) {
	ctx := t.Context()
	r := newRig()
	s := r.open(t, "ins_a")
	must(t, s.Create(ctx, newRun("run_1")))
	must(t, s.PutTranscript(ctx, "run_1", []byte("gz-1")))
	must(t, s.Compact(ctx))
	must(t, s.Create(ctx, newRun("run_2")))
	must(t, s.Delete(ctx, runKey("run_1")))

	creates, deletes := r.b.Calls(FakeCreate), r.b.Calls(FakeDelete)
	v, err := Read(ctx, r.b, r.opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.b.Calls(FakeCreate) != creates || r.b.Calls(FakeDelete) != deletes {
		t.Fatal("Read wrote to the bucket")
	}
	li := v.Loaded()
	// Above the snapshot: run_2's create, run_1's delete and its transcript record's.
	if li.Snapshot == 0 || li.Version != li.Snapshot+3 || li.LogEntries != 3 || li.Transcripts != 0 {
		t.Fatalf("loaded %+v", li)
	}
	log := v.Log()
	if len(log) != 3 || log[0].Op != memory.OpCreate || log[0].Name != "run_2" || log[1].Op != memory.OpDelete ||
		log[1].Name != "run_1" || log[2].Kind != KindTranscriptBlob || log[2].Version != li.Version {
		t.Fatalf("log %+v", log)
	}
	var runs []*v1alpha1.Run
	if _, err := v.List(ctx, v1alpha1.KindRun, store.ListOptions{}, &runs); err != nil || len(runs) != 1 || runs[0].Metadata.Name != "run_2" {
		t.Fatalf("runs %v %v", runs, err)
	}
	if err := v.Get(ctx, runKey("run_1"), &v1alpha1.Run{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a deleted run: %v", err)
	}
	// run_1's delete dropped its transcript; put one for run_2 and read it through a new view.
	must(t, s.PutTranscript(ctx, "run_2", []byte("gz-2")))
	v, err = Read(ctx, r.b, r.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v.Transcript(ctx, "run_2"); err != nil || !bytes.Equal(got, []byte("gz-2")) {
		t.Fatalf("transcript %q %v", got, err)
	}
	if _, err := v.Transcript(ctx, "run_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a dropped transcript: %v", err)
	}
}

// TestViewPastReadableSchema: an entry a release can't read ends the view, with a warning.
func TestViewPastReadableSchema(t *testing.T) {
	ctx := t.Context()
	r := newRig()
	s := r.open(t, "ins_new", withSchema(store.SchemaVersion+1, store.ReadableSchema+1))
	must(t, s.Create(ctx, newRun("run_1")))
	v, err := Read(ctx, r.b, r.opts)
	if err != nil {
		t.Fatal(err)
	}
	if li := v.Loaded(); !li.ReadOnly || li.Version != 0 || len(v.Warnings()) != 1 {
		t.Fatalf("loaded %+v, warnings %v", li, v.Warnings())
	}
}
