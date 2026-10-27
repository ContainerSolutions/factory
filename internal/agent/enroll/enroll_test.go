package enroll

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(t.TempDir(), "enrollment-token")
	f := Files{StateDir: filepath.Join(dir, "state"), TokenFile: tokFile}

	if _, _, err := (Files{StateDir: f.StateDir}).Token(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("no files: %v", err)
	}
	if err := os.WriteFile(tokFile, []byte("enr_secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Token(); err == nil || strings.Contains(err.Error(), "enr_secret") {
		t.Fatalf("a token file others can read: %v", err)
	}
	if err := os.Chmod(tokFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, refresh, err := f.Token(); err != nil || tok != "enr_secret" || refresh || f.Enrolled() {
		t.Fatalf("the enrollment token: %q %v %v", tok, refresh, err)
	}

	for _, next := range []string{"arf_first", "arf_second"} {
		if err := f.Save(next); err != nil {
			t.Fatal(err)
		}
		if tok, refresh, err := f.Token(); err != nil || tok != next || !refresh || !f.Enrolled() {
			t.Fatalf("after Save(%s): %q %v %v", next, tok, refresh, err)
		}
	}
	fi, err := os.Stat(filepath.Join(f.StateDir, RefreshFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the refresh file: %v %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(f.StateDir, ".*tmp*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	if err := f.Forget(); err != nil || f.Enrolled() {
		t.Fatalf("Forget: %v", err)
	}
	if tok, refresh, err := f.Token(); err != nil || tok != "enr_secret" || refresh {
		t.Fatalf("after Forget: %q %v %v", tok, refresh, err)
	}
	if err := f.Forget(); err != nil {
		t.Fatalf("Forget twice: %v", err)
	}
}
