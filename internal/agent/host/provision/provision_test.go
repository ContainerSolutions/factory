package provision

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// TestWriteKataContext writes the embedded context: exactly the five files, the scripts 0755,
// their embedded bytes, and the sha256 KataContextSHA256 gives, which sha256sum's manifest
// reproduces.
func TestWriteKataContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ctx")
	sum, err := WriteKataContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{"Dockerfile", "base.sh", "factory-docker-storage", "factory-docker-storage.service", "factory-init"}
	if !slices.Equal(names, want) {
		t.Fatalf("files %v, want %v", names, want)
	}
	modes := map[string]fs.FileMode{"Dockerfile": 0o644, "base.sh": 0o755, "factory-docker-storage": 0o755, "factory-docker-storage.service": 0o644, "factory-init": 0o755}
	var manifest strings.Builder
	for _, n := range want {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != modes[n] {
			t.Errorf("%s: mode %v, want %v", n, fi.Mode().Perm(), modes[n])
		}
		got, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		emb, err := fs.ReadFile(embedded, kataContext[n])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, emb) {
			t.Errorf("%s differs from the embedded file", n)
		}
		s := sha256.Sum256(got)
		fmt.Fprintf(&manifest, "%s  %s\n", hex.EncodeToString(s[:]), n)
	}
	m := sha256.Sum256([]byte(manifest.String()))
	if hex.EncodeToString(m[:]) != sum {
		t.Errorf("sum %s, want sha256sum's manifest's %s", sum, hex.EncodeToString(m[:]))
	}
	again, err := KataContextSHA256()
	if err != nil || again != sum {
		t.Errorf("KataContextSHA256() = %s, %v; want %s", again, err, sum)
	}

	// a second write over the first (the files' modes changed meanwhile) restores them.
	if err := os.Chmod(filepath.Join(dir, "factory-init"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := WriteKataContext(dir); err != nil || again != sum {
		t.Fatalf("rewrite: %s, %v", again, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "factory-init")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("rewrite left factory-init at %v (%v)", fi.Mode().Perm(), err)
	}
}

// TestKataContextEmbedsEveryFile: every file under kata/ is in the context, and the
// Dockerfile uses every other file of the context by its name.
func TestKataContextEmbedsEveryFile(t *testing.T) {
	ents, err := fs.ReadDir(embedded, "kata")
	if err != nil {
		t.Fatal(err)
	}
	var inDir []string
	for _, e := range ents {
		inDir = append(inDir, "kata/"+e.Name())
	}
	var inCtx []string
	for _, src := range kataContext {
		if strings.HasPrefix(src, "kata/") {
			inCtx = append(inCtx, src)
		}
	}
	slices.Sort(inCtx)
	if !slices.Equal(inDir, inCtx) {
		t.Errorf("kata/ has %v, the context %v", inDir, inCtx)
	}
	df, err := fs.ReadFile(embedded, "kata/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for n := range kataContext {
		if n == "Dockerfile" {
			continue
		}
		if !regexp.MustCompile(`(?m)^(COPY|RUN)\b.*[ =]` + regexp.QuoteMeta(n) + `[ ,]`).Match(df) {
			t.Errorf("the Dockerfile doesn't use %s", n)
		}
	}
}

// TestContextSHA256 is stable and changes when any file's content or name changes.
func TestContextSHA256(t *testing.T) {
	fsys := fstest.MapFS{}
	files := map[string]string{}
	for n, src := range kataContext {
		fsys[src] = &fstest.MapFile{Data: []byte("content of " + n + "\n")}
		files[n] = src
	}
	base, err := contextSHA256(fsys, files)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := contextSHA256(fsys, files); again != base {
		t.Fatalf("unstable: %s then %s", base, again)
	}
	for n, src := range kataContext {
		t.Run(n, func(t *testing.T) {
			changed := maps.Clone(fsys)
			changed[src] = &fstest.MapFile{Data: []byte("content of " + n + "!\n")}
			if s, err := contextSHA256(changed, files); err != nil || s == base {
				t.Errorf("a change to %s's content: %s, %v", n, s, err)
			}
			renamed := maps.Clone(files)
			delete(renamed, n)
			renamed[n+".x"] = src
			if s, err := contextSHA256(fsys, renamed); err != nil || s == base {
				t.Errorf("a rename of %s: %s, %v", n, s, err)
			}
		})
	}
	if _, err := contextSHA256(fsys, map[string]string{"missing": "kata/missing"}); err == nil {
		t.Error("a missing file: no error")
	}
}

// TestKataScripts: the context's shell scripts parse (sh -n; bash -n for base.sh).
func TestKataScripts(t *testing.T) {
	for _, c := range []struct{ shell, src string }{
		{"sh", "kata/factory-init"},
		{"sh", "kata/factory-docker-storage"},
		{"bash", "base.sh"},
	} {
		sh, err := exec.LookPath(c.shell)
		if err != nil {
			t.Logf("no %s: skipping %s", c.shell, c.src)
			continue
		}
		b, err := fs.ReadFile(embedded, c.src)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), filepath.Base(c.src))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(sh, "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s -n %s: %v\n%s", c.shell, c.src, err, out)
		}
	}
}
