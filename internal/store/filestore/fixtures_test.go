package filestore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/clock"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/scheduler"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/storeaudit"
)

var jsonUnmarshal = json.Unmarshal

// withInternalKinds registers the internal kinds the dispatcher stores, with their real types.
func withInternalKinds() Option {
	return func(c *config) {
		for kind, proto := range storeaudit.InternalKinds() {
			WithKind(kind, proto)(c)
		}
	}
}

// kindAudit is every kind the stores hold, with why its stored form carries no secret
// (storeaudit.KindAudit, shared with the object store's audit).
var kindAudit = storeaudit.KindAudit

// storedFile is one state file's bytes.
type storedFile struct {
	path string
	data []byte
}

// readStored returns the snapshot and journal of dir (the transcripts are agent output, not
// store objects).
func readStored(t *testing.T, dir string) []storedFile {
	t.Helper()
	var out []storedFile
	for _, f := range []string{snapshotFile, journalFile} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, storedFile{path: filepath.Join(dir, f), data: b})
	}
	return out
}

// storedKinds returns the kinds in the files.
func storedKinds(t *testing.T, files []storedFile) map[string]bool {
	t.Helper()
	kinds := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f.path, snapshotFile) {
			var snap struct {
				Objects []snapshotObject `json:"objects"`
			}
			if err := json.Unmarshal(f.data, &snap); err != nil {
				t.Fatalf("%s: %v", f.path, err)
			}
			for _, o := range snap.Objects {
				kinds[o.Kind] = true
			}
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(f.data))
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			var jl journalLine
			if err := json.Unmarshal(sc.Bytes(), &jl); err != nil {
				t.Fatalf("%s: %v", f.path, err)
			}
			kinds[jl.Kind] = true
		}
	}
	return kinds
}

func checkKindAudit(t *testing.T, files []storedFile) {
	t.Helper()
	kinds := storedKinds(t, files)
	if len(kinds) == 0 {
		t.Fatal("nothing stored")
	}
	for k := range kinds {
		if kindAudit[k] == "" {
			t.Errorf("kind %s reaches the disk with no entry in kindAudit: check that its stored form holds no credential and add a line saying why", k)
		}
	}
	for _, k := range builtinKinds {
		if kindAudit[k] == "" {
			t.Errorf("built-in kind %s has no entry in kindAudit", k)
		}
	}
}

// findTokens is storeaudit.FindTokens.
func findTokens(data []byte) []string { return storeaudit.FindTokens(data) }

func checkNoTokens(t *testing.T, files []storedFile) {
	t.Helper()
	for _, f := range files {
		if hits := findTokens(f.data); len(hits) > 0 {
			t.Errorf("%s holds token-shaped strings: %v", f.path, hits)
		}
	}
}

// TestSecretsFixture stores a real-shaped object of every audited kind, as the dispatcher
// builds them, and scans the journal and the snapshot.
func TestSecretsFixture(t *testing.T) {
	dir := stateDir(t)
	opts, clk := testOpts()
	s := mustOpen(t, dir, opts)
	writeMix(t, s, clk)
	files := readStored(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	files = append(files, readStored(t, dir)...)

	kinds := storedKinds(t, files)
	for k := range kindAudit {
		if !kinds[k] {
			t.Errorf("audited kind %s is not in the fixture: add a real-shaped one to fixtures()", k)
		}
	}
	checkKindAudit(t, files)
	checkNoTokens(t, files)

	// The scanner finds what it looks for, also inside a base64 body.
	planted := delivery("gh-planted")
	planted.Spec.Headers.Set("X-Leak", "token ghs_0123456789abcdef")
	planted.Spec.Body = []byte(`{"token":"github_pat_11ABC"}`)
	b, err := json.Marshal(planted)
	if err != nil {
		t.Fatal(err)
	}
	hits := strings.Join(findTokens(b), ",")
	if !strings.Contains(hits, "ghs_") || !strings.Contains(hits, "base64 github_pat_") {
		t.Fatalf("scanner missed the planted tokens: %q", hits)
	}
}

// ---- fixtures --------------------------------------------------------------------------------

var (
	run      = storeaudit.Run
	task     = storeaudit.Task
	lease    = storeaudit.Lease
	delivery = storeaudit.Delivery
)

// fixtures returns one real-shaped object of every kind the dispatcher stores.
func fixtures() []store.Object {
	return storeaudit.Fixtures()
}

// writeMix writes creates, updates and deletes across every kind, transcripts, and holds a
// lock, advancing the clock between writes.
func writeMix(t *testing.T, s *Store, clk *clock.Fake) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range fixtures() {
		must(s.Create(ctx, o))
		clk.Advance(time.Second)
	}
	get := func(k store.Key, out store.Object) {
		t.Helper()
		must(s.Get(ctx, k, out))
	}

	// A task through its phases to a terminal one (retention from its last write).
	var tk v1alpha1.Task
	get(store.Key{Kind: v1alpha1.KindTask, Name: "tsk_1"}, &tk)
	for _, p := range []v1alpha1.TaskPhase{v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskSucceeded} {
		tk.Status.Phase = p
		must(s.Update(ctx, &tk))
		clk.Advance(time.Minute)
	}
	// A work item gains a link (a field index).
	var wi v1alpha1.WorkItem
	get(store.Key{Kind: v1alpha1.KindWorkItem, Name: "wi-1"}, &wi)
	wi.Spec.Links = append(wi.Spec.Links, v1alpha1.Link{Provider: "github-public", Ref: "compliance-framework/mock-docs#5", Kind: "pr"})
	must(s.Update(ctx, &wi))
	// An Event (no metadata) is updated unconditionally.
	var ev v1alpha1.Event
	get(store.Key{Kind: v1alpha1.KindEvent, Name: "gh-1/0"}, &ev)
	ev.Subject = "compliance-framework/mock-docs#4 (edited)"
	must(s.Update(ctx, &ev))
	// A lease heartbeats; the budget counts.
	var ls scheduler.Lease
	get(store.Key{Kind: scheduler.KindLease, Name: "lse_1"}, &ls)
	ls.Status.LastHeartbeat, ls.Status.Usage.InputTokens = clk.Now(), 1000
	must(s.Update(ctx, &ls))
	var bc v1alpha1.BudgetCounter
	get(store.Key{Kind: v1alpha1.KindBudgetCounter, Name: "budget.global.all.2026-01-01"}, &bc)
	bc.Status.Runs++
	must(s.Update(ctx, &bc))
	// A handled delivery drops its body and headers.
	var d controllers.Delivery
	get(store.Key{Kind: controllers.KindDelivery, Name: "gh-1"}, &d)
	d.Spec.Body, d.Spec.Headers = nil, nil
	d.Status = controllers.DeliveryStatus{Phase: controllers.DeliveryNormalised, Events: []string{"gh-1/0"}, HandledAt: clk.Now()}
	must(s.Update(ctx, &d))

	// Transcripts, one with a name that isn't a file name.
	must(s.PutTranscript(ctx, "run_1", []byte("\x1f\x8bgzip-bytes")))
	clk.Advance(time.Second)
	must(s.PutTranscript(ctx, "run_gone", []byte("gone")))
	must(s.PutTranscript(ctx, "odd/name", []byte("odd")))

	// Deletes: a run with its transcript, a debounce record; a deleted name is reused.
	must(s.Delete(ctx, store.Key{Kind: v1alpha1.KindRun, Name: "run_gone"}))
	must(s.Delete(ctx, store.Key{Kind: controllers.KindDebounce, Name: "deb_1"}))
	must(s.Delete(ctx, store.Key{Kind: v1alpha1.KindTask, Name: "tsk_2"}))
	clk.Advance(time.Second)
	must(s.Create(ctx, task("tsk_2", "wi-1", v1alpha1.TaskPending)))

	// Locks are not journaled.
	u, err := s.Lock(ctx, "held-before", time.Hour)
	must(err)
	_ = u

	s.tmu.Lock()
	n := len(s.blobs)
	s.tmu.Unlock()
	if n != 2 {
		t.Fatalf("transcripts after the mix = %v, want 2 (run_1 and odd/name)", n)
	}
}
