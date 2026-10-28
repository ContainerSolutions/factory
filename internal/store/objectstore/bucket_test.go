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

// TestBuckets: the fake and the file bucket behave alike: create-only creates, listing in name
// order after startAfter, deletes and not-exist errors.
func TestBuckets(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) Bucket{
		"fake": func(*testing.T) Bucket { return NewFakeBucket(nil) },
		"file": func(t *testing.T) Bucket {
			b, err := NewFileBucket(filepath.Join(t.TempDir(), "bucket"))
			must(t, err)
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, ctx := open(t), t.Context()
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
			for _, n := range []string{"log/3", "log/2", "snapshots/9"} {
				must(t, b.Create(ctx, n, []byte(n)))
			}
			var names []string
			must(t, b.List(ctx, "log/", "log/1", func(o ObjectInfo) error {
				names = append(names, o.Name)
				if o.Size != int64(len(o.Name)) {
					t.Errorf("%s size %d", o.Name, o.Size)
				}
				return nil
			}))
			if !slices.Equal(names, []string{"log/2", "log/3"}) {
				t.Fatalf("List = %v", names)
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
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := b.Get(cancelled, "log/2"); !errors.Is(err, context.Canceled) {
				t.Fatalf("Get with a cancelled context = %v", err)
			}
		})
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
		b, err := ParseURL(u)
		must(t, err)
		if fb, ok := b.(*FileBucket); !ok || fb.Dir() != dir {
			t.Fatalf("ParseURL(%s) = %#v", u, b)
		}
	}
	// Each refusal names the part that is wrong.
	for u, want := range map[string]string{
		"file://host/dir":          "the URL has a host",
		"file:relative":            "the URL has a relative path",
		"file://localhost":         "the URL has a relative path",
		"file://" + dir + "?x=1":   "the URL has a query",
		"file://" + dir + "#f":     "the URL has a fragment",
		"file://u@localhost" + dir: "the URL has userinfo",
		"::":                       "the state URL doesn't parse",
		"nope://b/p":               `unknown scheme "nope" (known: file://`,
	} {
		if _, err := ParseURL(u); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("ParseURL(%q) = %v, want an error saying %q", u, err, want)
		}
	}
	// A URL may carry a credential anywhere, and one holding '/', '?', '#' or '"' doesn't
	// parse as userinfo: no error prints any of the URL but its scheme.
	for _, u := range []string{
		"s3a://AKIAEXAMPLE:hunter2@bucket/p",     // unknown scheme
		"s3a://hunter2@bucket/p",                 // the user alone is the secret
		"s3a://AKIAEXAMPLE:/hunter2XYZ@bucket/p", // the secret starts with '/': host "AKIAEXAMPLE:", secret in the path
		`s3://AKIA:se"hunter2XYZ/rest@bucket/p`,  // a quote and a '/' in the secret
		"s3://AKIA:hun#ter2XYZ@bucket/p",         // a '#': the secret's tail is the fragment
		"s3://AKIA:hunter2?XYZ@bucket/p",         // a '?': the query
		"s3://AKIAEXAMPLE:hunter2%zz@bucket/p",   // a bad escape in the userinfo
		"gs://AKIAEXAMPLE:hunter2@bu cket/p",     // a bad host
		"s3://AKIAEXAMPLE:hunter2@bucket:port/p", // a bad port
		"s3://AKIAEXAMPLE:hun ter2@bucket/p",     // bad userinfo
		"file://u:/hunter2XYZ@localhost/abs/dir", // the reviewer's file:// case
		"file://u:hunter2@localhost" + dir,       // userinfo on file://
		"file://hunter2@localhost" + dir,         // the user alone, on file://
		"file://hunter2@host" + dir,              // and with a host file:// refuses
		"file://localhost/" + dir + "?hunter2",   // in the query
	} {
		_, err := ParseURL(u)
		if err == nil {
			t.Fatalf("ParseURL(%q) was accepted", u)
		}
		for _, secret := range []string{"hunter2XYZ", "hunter2", "hun ter2", "hun#ter2"} {
			for n := 3; n <= len(secret); n++ {
				for i := 0; i+n <= len(secret); i++ {
					if sub := secret[i : i+n]; strings.Contains(err.Error(), sub) {
						t.Fatalf("ParseURL(%q)'s error holds %q of the secret: %v", u, sub, err)
					}
				}
			}
		}
	}

	fake := NewFakeBucket(nil)
	RegisterScheme("memtest", func(*url.URL) (Bucket, error) { return fake, nil })
	if b, err := ParseURL("memtest://bucket/prefix"); err != nil || b != fake {
		t.Fatalf("registered scheme = %v, %v", b, err)
	}
}
