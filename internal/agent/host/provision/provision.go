// Package provision holds the agent hosts' workspace bases: base.sh, the provisioning script
// the tart base VM runs (package host embeds it too), and the kata driver's image, whose build
// context is kata/ plus base.sh (design/workstreams/agent/LLD/workspace-kata.md, "The image").
package provision

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

//go:embed base.sh kata
var embedded embed.FS

// kataContext maps each file of the kata image's build context, by its name in the context, to
// its path in embedded. The Dockerfile COPYs (or mounts) the others by these names.
var kataContext = map[string]string{
	"Dockerfile":                     "kata/Dockerfile",
	"base.sh":                        "base.sh",
	"factory-init":                   "kata/factory-init",
	"factory-docker-storage":         "kata/factory-docker-storage",
	"factory-docker-storage.service": "kata/factory-docker-storage.service",
}

// kataScripts are the context's executables, written 0755; the rest are 0644.
var kataScripts = []string{"base.sh", "factory-init", "factory-docker-storage"}

// KataContextSHA256 is the kata image's build context's sha256 (katabase.json's
// contextSHA256): the sha256 of `sha256sum`'s lines for its files, sorted by name, so any
// change to a file's name or content changes it. In a written context,
// `LC_ALL=C bash -c 'sha256sum * | sha256sum'` gives the same (the glob sorted bytewise).
func KataContextSHA256() (string, error) {
	return contextSHA256(embedded, kataContext)
}

// WriteKataContext writes the kata image's build context into dir, flat (the directory is made
// when missing, and files of the same names are replaced): the Dockerfile, base.sh,
// factory-init, factory-docker-storage and its unit, the scripts 0755 and the rest 0644. It
// returns the context's sha256 (KataContextSHA256).
func WriteKataContext(dir string) (string, error) {
	return writeContext(embedded, kataContext, dir)
}

func writeContext(fsys fs.FS, files map[string]string, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("the kata build context: %w", err)
	}
	for name, src := range files {
		b, err := fs.ReadFile(fsys, src)
		if err != nil {
			return "", fmt.Errorf("the kata build context: %w", err)
		}
		mode := fs.FileMode(0o644)
		if slices.Contains(kataScripts, name) {
			mode = 0o755
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, mode); err != nil {
			return "", fmt.Errorf("the kata build context: %w", err)
		}
		// WriteFile's mode is masked by the umask, and an existing file keeps its own.
		if err := os.Chmod(p, mode); err != nil {
			return "", fmt.Errorf("the kata build context: %w", err)
		}
	}
	return contextSHA256(fsys, files)
}

func contextSHA256(fsys fs.FS, files map[string]string) (string, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, n := range names {
		b, err := fs.ReadFile(fsys, files[n])
		if err != nil {
			return "", fmt.Errorf("the kata build context: %w", err)
		}
		s := sha256.Sum256(b)
		fmt.Fprintf(h, "%s  %s\n", hex.EncodeToString(s[:]), n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
