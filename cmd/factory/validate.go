package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/catalog/gitsource"
)

// Exit codes of factory validate (catalog LLD §factory validate).
const (
	exitValid   = 0
	exitInvalid = 1
	exitUsage   = 2
)

func validateCommand() command {
	return command{
		name:    "validate",
		summary: "load and validate a catalog (a directory, or a git ref)",
		run:     runValidate,
	}
}

// validateReport is the --format json output.
type validateReport struct {
	SHA         string          `json:"sha,omitzero"`
	Valid       bool            `json:"valid"`
	Errors      int             `json:"errors"`
	Warnings    int             `json:"warnings"`
	Diagnostics []catalog.Error `json:"diagnostics"`
}

func runValidate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("factory validate", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dir := fl.String("dir", "catalog", "catalog directory; with --git, the directory inside the repository")
	gitURL := fl.String("git", "", "load from this git repository instead of a local directory")
	ref := fl.String("ref", "main", "with --git: the branch, tag or commit to load")
	capsFile := fl.String("capabilities", "", "harness adapter capabilities (JSON); default: the embedded descriptors")
	modelsFile := fl.String("known-models", "", "known model ids by harness (JSON); default: the embedded list")
	format := fl.String("format", "text", "output format: text or json")
	strict := fl.Bool("strict", false, "treat warnings as errors")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: factory validate [--dir catalog] [--git URL --ref REF] [--capabilities caps.json] [--known-models models.json] [--format text|json] [--strict]")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitValid
		}
		return exitUsage
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "factory validate: unexpected arguments %v\n", fl.Args())
		return exitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "factory validate: --format must be text or json, not %q\n", *format)
		return exitUsage
	}

	opts := catalog.Options{Strict: *strict}
	if *capsFile != "" {
		data, err := os.ReadFile(*capsFile)
		if err == nil {
			opts.Capabilities, err = catalog.ParseCapabilities(data)
		}
		if err != nil {
			fmt.Fprintf(stderr, "factory validate: --capabilities: %v\n", err)
			return exitUsage
		}
	}
	if *modelsFile != "" {
		data, err := os.ReadFile(*modelsFile)
		if err == nil {
			opts.KnownModels, err = catalog.ParseKnownModels(data)
		}
		if err != nil {
			fmt.Fprintf(stderr, "factory validate: --known-models: %v\n", err)
			return exitUsage
		}
	}

	var (
		c    *catalog.Catalog
		errs []catalog.Error
	)
	if *gitURL != "" {
		var err error
		c, errs, err = gitsource.Load(ctx, *gitURL, *ref, *dir, opts)
		if err != nil {
			fmt.Fprintf(stderr, "factory validate: %v\n", err)
			return exitUsage
		}
	} else {
		info, err := os.Stat(*dir)
		if err != nil || !info.IsDir() {
			fmt.Fprintf(stderr, "factory validate: %s is not a directory\n", *dir)
			return exitUsage
		}
		c, errs = catalog.LoadDir(os.DirFS(*dir), opts)
	}

	rep := validateReport{Diagnostics: catalogDiagnostics(*dir, errs)}
	if c != nil {
		rep.SHA = c.SHA
	}
	for _, e := range rep.Diagnostics {
		if e.Severity == catalog.SeverityError {
			rep.Errors++
		} else {
			rep.Warnings++
		}
	}
	rep.Valid = rep.Errors == 0

	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "factory validate: %v\n", err)
			return exitUsage
		}
	} else {
		for _, e := range rep.Diagnostics {
			fmt.Fprintln(stdout, e.Error())
		}
		status := "valid"
		if !rep.Valid {
			status = "invalid"
		}
		sha := ""
		if rep.SHA != "" {
			sha = " (" + rep.SHA + ")"
		}
		fmt.Fprintf(stderr, "%s: %s%s: %d errors, %d warnings\n", *dir, status, sha, rep.Errors, rep.Warnings)
	}
	if !rep.Valid {
		return exitInvalid
	}
	return exitValid
}

// catalogDiagnostics are errs with their files relative to the working directory (dir/file),
// as factory validate prints them.
func catalogDiagnostics(dir string, errs []catalog.Error) []catalog.Error {
	out := make([]catalog.Error, len(errs))
	for i, e := range errs {
		e.File = filepath.ToSlash(filepath.Join(dir, e.File))
		out[i] = e
	}
	return out
}
