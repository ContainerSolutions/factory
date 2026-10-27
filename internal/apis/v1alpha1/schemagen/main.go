// Command schemagen writes the JSON Schemas of the v1alpha1 config kinds and of Result:
// one <kind>.schema.json per kind, plus result.schema.json. It removes stale *.schema.json
// files from the output directory. `make schemas` runs it.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

func main() {
	out := flag.String("out", "schemas", "output directory")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	files, err := v1alpha1.Schemas()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	existing, err := filepath.Glob(filepath.Join(dir, "*.schema.json"))
	if err != nil {
		return err
	}
	for _, path := range existing {
		if _, keep := files[filepath.Base(path)]; !keep {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove stale %s: %w", path, err)
			}
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	fmt.Printf("schemagen: wrote %d schemas to %s (%s)\n", len(names), dir, strings.Join(names, ", "))
	return nil
}
