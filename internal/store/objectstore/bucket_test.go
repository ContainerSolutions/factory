package objectstore

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/store/filestore"
)

// TestBuckets: the fake and the file bucket pass the bucket conformance (testBucket).
func TestBuckets(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) Bucket{
		"fake": func(*testing.T) Bucket { return NewFakeBucket(nil) },
		"file": func(t *testing.T) Bucket {
			b, err := NewFileBucket(filepath.Join(t.TempDir(), "bucket"))
			must(t, err)
			return b
		},
	} {
		t.Run(name, func(t *testing.T) { testBucket(t, open(t)) })
	}
}

// testBucket is the bucket conformance every Bucket passes, on an empty bucket: create-only
// creates, listing in name order after startAfter with sizes and times,
// deletes and not-exist errors, and a cancelled context's error. The GCS and S3 buckets run
// it against their emulators (emulator_test.go).
func testBucket(t *testing.T, b Bucket) {
	t.Helper()
	ctx := t.Context()
	if _, err := b.Get(ctx, "log/1"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get of nothing = %v", err)
	}
	must(t, b.Create(ctx, "log/1", []byte("one")))
	if err := b.Create(ctx, "log/1", []byte("two")); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}
	if got, err := b.Get(ctx, "log/1"); err != nil || string(got) != "one" {
		t.Fatalf("Get = %q, %v: the second Create changed it", got, err)
	}
	for _, n := range []string{"log/3", "log/2", "snapshots/9", "transcripts/r.jsonl.gz"} {
		must(t, b.Create(ctx, n, []byte(n)))
	}
	var names []string
	must(t, b.List(ctx, "log/", "log/1", func(o ObjectInfo) error {
		names = append(names, o.Name)
		if o.Size != int64(len(o.Name)) {
			t.Errorf("%s size %d", o.Name, o.Size)
		}
		if o.Updated.IsZero() {
			t.Errorf("%s has no Updated time", o.Name)
		}
		return nil
	}))
	if !slices.Equal(names, []string{"log/2", "log/3"}) {
		t.Fatalf("List = %v", names)
	}
	names = nil
	must(t, b.List(ctx, "", "", func(o ObjectInfo) error { names = append(names, o.Name); return nil }))
	if want := []string{"log/1", "log/2", "log/3", "snapshots/9", "transcripts/r.jsonl.gz"}; !slices.Equal(names, want) {
		t.Fatalf("List of everything = %v, want %v", names, want)
	}
	stop := errors.New("stop")
	if err := b.List(ctx, "", "", func(ObjectInfo) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("List's fn error = %v", err)
	}
	must(t, b.List(ctx, "nothing/", "", func(o ObjectInfo) error { t.Fatalf("listed %s", o.Name); return nil }))
	must(t, b.Delete(ctx, "log/1"))
	if err := b.Delete(ctx, "log/1"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("second Delete = %v", err)
	}
	if _, err := b.Get(ctx, "log/1"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get after Delete = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.Get(cancelled, "log/2"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get with a cancelled context = %v", err)
	}
}

// TestFileBucketGuards: a file:// bucket refuses a P2 state dir (in both directions: the file
// store refuses a bucket's dir), a relative path, a dir others can read, and names that would
// leave the dir.
func TestFileBucketGuards(t *testing.T) {
	ctx := t.Context()
	for _, f := range []string{"snapshot.json", "journal.jsonl", "LOCK"} {
		dir := filepath.Join(t.TempDir(), "state")
		must(t, os.MkdirAll(dir, 0o700))
		must(t, os.WriteFile(filepath.Join(dir, f), nil, 0o600))
		if _, err := NewFileBucket(dir); err == nil || !strings.Contains(err.Error(), "P2 state dir") {
			t.Fatalf("a dir with %s = %v, want refused as a P2 state dir", f, err)
		}
	}
	// A real P2 dir, and the other way round.
	p2 := filepath.Join(t.TempDir(), "p2")
	opts, _ := testOpts()
	fs, err := filestore.Open(p2, opts)
	must(t, err)
	if _, err := NewFileBucket(p2); err == nil {
		t.Fatal("a P2 state dir opened as a bucket")
	}
	must(t, fs.Close())
	bucketDir := filepath.Join(t.TempDir(), "bucket")
	b, err := NewFileBucket(bucketDir)
	must(t, err)
	s, err := Open(ctx, b, opts, withoutBackground())
	must(t, err)
	must(t, s.Create(ctx, newRun("r1")))
	must(t, s.Compact(ctx))
	must(t, s.Close())
	if _, err := filestore.Open(bucketDir, opts); !errors.Is(err, filestore.ErrObjectStoreDir) {
		t.Fatalf("filestore.Open on a bucket's dir = %v, want ErrObjectStoreDir", err)
	}

	if _, err := NewFileBucket("relative/dir"); err == nil {
		t.Fatal("a relative path was accepted")
	}
	open := filepath.Join(t.TempDir(), "open")
	must(t, os.MkdirAll(open, 0o755))
	must(t, os.Chmod(open, 0o755))
	if _, err := NewFileBucket(open); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("a 0755 dir = %v, want refused with the chmod", err)
	}
	for _, name := range []string{"../x", "log/../../x", "/abs", ".tmp/x", "log//x", ""} {
		if err := b.Create(ctx, name, nil); err == nil {
			t.Fatalf("Create(%q) was accepted", name)
		}
	}
}

// TestParseURL: file:// opens a FileBucket, other schemes come from the registry, and an
// unknown scheme names the known ones.
func TestParseURL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bucket")
	for _, u := range []string{"file://" + dir, "file://localhost" + dir} {
		b, err := ParseURL(t.Context(), u)
		must(t, err)
		if fb, ok := b.(*FileBucket); !ok || fb.Dir() != dir {
			t.Fatalf("ParseURL(%s) = %#v", u, b)
		}
	}
	for _, u := range []string{"file://host/dir", "file:relative", "file://" + dir + "?x=1", "::"} {
		if _, err := ParseURL(t.Context(), u); err == nil {
			t.Fatalf("ParseURL(%q) was accepted", u)
		}
	}
	if _, err := ParseURL(t.Context(), "nope://b/p"); err == nil || !strings.Contains(err.Error(), "file://") {
		t.Fatalf("unknown scheme = %v, want an error naming file://", err)
	}
	fake := NewFakeBucket(nil)
	RegisterScheme("memtest", func(context.Context, *url.URL) (Bucket, error) { return fake, nil })
	if b, err := ParseURL(t.Context(), "memtest://bucket/prefix"); err != nil || b != fake {
		t.Fatalf("registered scheme = %v, %v", b, err)
	}
}
