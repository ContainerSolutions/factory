package host

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/agent/host/provision"
	"github.com/compliance-framework/factory/internal/agent/katahelper"
)

// The kata driver's image step (workspace-kata.md, step 9): the workspace image, built by the
// factory's BuildKit through the helper from the build context this binary embeds.

const kataImageName = "kataimage"

// kataImageTimeout bounds the helper's image-build: its own nerdctl build bound is 90 minutes,
// so this one ends only a helper that hangs past it (receiving the context, or its cleanup).
const kataImageTimeout = 2 * time.Hour

// KataBaseRecord is <statedir>/katabase.json: the workspace image host init built.
type KataBaseRecord struct {
	Image string `json:"image"`
	// ContextSHA256 is the build context's (provision.KataContextSHA256): a change to any of
	// its files rebuilds the image under the same tag.
	ContextSHA256 string `json:"contextSHA256"`
	BuiltAt       string `json:"builtAt"`
}

// KataImage is this factory-agent's workspace image: factory-agent-base:<version>, the version
// as BaseVMName has it (no leading v, [^A-Za-z0-9._-] as _).
func (e *Env) KataImage() string {
	return katahelper.ImageRepo + ":" + nameUnsafeRE.ReplaceAllString(strings.TrimPrefix(e.AgentVersion, "v"), "_")
}

// KataBaseRecordPath is katabase.json, which supervise reads the image from.
func (e *Env) KataBaseRecordPath() string { return filepath.Join(e.StateDir(), "katabase.json") }

// ReadKataBase reads katabase.json: the image supervise's kata driver creates workspaces from.
func (e *Env) ReadKataBase() (KataBaseRecord, error) {
	var r KataBaseRecord
	ok, err := readRecord(e.KataBaseRecordPath(), &r)
	switch {
	case err != nil:
		return r, err
	case !ok || r.Image == "":
		return r, fmt.Errorf("%s names no image: run `factory-agent host init --driver kata`", e.KataBaseRecordPath())
	}
	return r, nil
}

// helperArgv is the command that runs the installed helper's verb with sudo -n.
func (e *Env) helperArgv(args ...string) []string {
	return append([]string{"-n", e.sys(katahelper.HelperPath)}, args...)
}

// helperOK runs the helper's verb with sudo -n, its output kept for the error: the helper's
// own last line (kata-helper <verb>: …) when it printed one, else sudo's.
func (e *Env) helperOK(ctx context.Context, args ...string) error {
	var b bytes.Buffer
	if err := e.Runner.Run(ctx, &b, "sudo", e.helperArgv(args...)...); err != nil {
		l := lastOutputLine(b.String())
		if strings.HasPrefix(l, "kata-helper ") {
			return errors.New(l)
		}
		if l != "" {
			err = fmt.Errorf("%w: %s", err, l)
		}
		return fmt.Errorf("kata-helper %s: %w", args[0], err)
	}
	return nil
}

func lastOutputLine(s string) string {
	s = strings.TrimSpace(s)
	return strings.TrimSpace(s[strings.LastIndex(s, "\n")+1:])
}

// kataImageStep builds the workspace image.
type kataImageStep struct{ e *Env }

func (kataImageStep) Name() string { return kataImageName }

func (s kataImageStep) Check(ctx context.Context) (Status, error) {
	e, p := s.e, s.e.KataBaseRecordPath()
	var got KataBaseRecord
	if ok, err := readRecord(p, &got); err != nil || !ok {
		return Status{State: StateMissing, Detail: "no " + p}, err
	}
	sum, err := provision.KataContextSHA256()
	if err != nil {
		return Status{}, err
	}
	for _, f := range []struct{ field, got, want string }{
		{"image", got.Image, e.KataImage()},
		{"contextSHA256", got.ContextSHA256, sum},
	} {
		if f.got != f.want {
			return Status{State: StateOutdated, Detail: fmt.Sprintf("%s: its %s is %s, want %s: host init rebuilds it", filepath.Base(p), f.field, f.got, f.want)}, nil
		}
	}
	if err := e.helperOK(ctx, "image-exists", got.Image); err != nil {
		return Status{State: StateMissing, Detail: fmt.Sprintf("%s isn't in the factory's containerd (%v)", got.Image, err)}, nil
	}
	return Status{State: StateOK, Detail: fmt.Sprintf("%s (built %s)", got.Image, got.BuiltAt)}, nil
}

// contextDir is where Apply writes the build context.
func (s kataImageStep) contextDir() string { return filepath.Join(s.e.StageDir, "kata-context") }

func (s kataImageStep) Plan(context.Context) (Plan, error) {
	e := s.e
	do := []string{
		"write the image's build context (the Dockerfile, base.sh and the image's scripts) into " + s.contextDir(),
		fmt.Sprintf("sudo -n %s image-build %s < the context as a tar: the factory's BuildKit builds it (Ubuntu and base.sh: many minutes)", e.sys(katahelper.HelperPath), e.KataImage()),
		"write " + e.KataBaseRecordPath(),
	}
	if prev := s.previous(); prev != "" {
		do = append(do, "kata-helper image-rm "+prev+" (the previous image, named in katabase.json), unless a workspace uses it")
	}
	return Plan{Do: do}, nil
}

// previous is the image katabase.json names when it's another of host init's (ImageRepo, not
// this binary's tag): a new version's build removes it. "" otherwise, also for an unreadable
// record.
func (s kataImageStep) previous() string {
	var rec KataBaseRecord
	if ok, err := readRecord(s.e.KataBaseRecordPath(), &rec); err != nil || !ok {
		return ""
	}
	if rec.Image == s.e.KataImage() || !strings.HasPrefix(rec.Image, katahelper.ImageRepo+":") {
		return ""
	}
	return rec.Image
}

func (s kataImageStep) Apply(ctx context.Context) error {
	e, tag := s.e, s.e.KataImage()
	dir := s.contextDir()
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	sum, err := provision.WriteKataContext(dir)
	if err != nil {
		return err
	}
	ctxTar, err := tarFlat(dir)
	if err != nil {
		return err
	}
	prev := s.previous()
	fmt.Fprintf(e.Out, "building %s with the factory's BuildKit (this takes a while)\n", tag)
	bctx, cancel := context.WithTimeout(ctx, kataImageTimeout)
	defer cancel()
	if err := e.Spawner.RunInput(bctx, bytes.NewReader(ctxTar), e.Out, "sudo", e.helperArgv("image-build", tag)...); err != nil {
		if ctx.Err() == nil && errors.Is(bctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w (timed out after %s)", err, kataImageTimeout)
		}
		return fmt.Errorf("kata-helper image-build: %w", err)
	}
	rec := KataBaseRecord{Image: tag, ContextSHA256: sum, BuiltAt: e.Clock.Now().UTC().Format(time.RFC3339)}
	if err := writeRecord(e.KataBaseRecordPath(), rec); err != nil {
		return err
	}
	if prev != "" {
		// nerdctl refuses while a workspace uses it: that's the warning.
		fmt.Fprintf(e.Out, "removing the previous image %s\n", prev)
		if err := e.helperOK(ctx, "image-rm", prev); err != nil {
			fmt.Fprintf(e.Out, "warning: the previous image %s is left (a workspace may still use it: sudo -n %s image-rm %s once none does): %v\n",
				prev, e.sys(katahelper.HelperPath), prev, err)
		}
	}
	return nil
}

// tarFlat is a tar of dir's regular files, by name, with their modes: the build context.
func tarFlat(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, ent := range entries {
		fi, err := ent.Info()
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("the build context's %s isn't a regular file", ent.Name())
		}
		body, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: ent.Name(), Mode: int64(fi.Mode().Perm()), Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
