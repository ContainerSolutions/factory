package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
	"github.com/ContainerSolutions/factory/internal/store/filestore"
	"github.com/ContainerSolutions/factory/internal/store/objectstore"
)

// envStateDir is the default --state-dir of factory state, as for deploy/poc/run.sh.
const envStateDir = "FACTORY_STATE_DIR"

// stateReadTimeout bounds factory state's read of a --state-url.
const stateReadTimeout = 2 * time.Minute

// runKitStateDir is how deploy/poc/run.sh picks the dispatcher's state dir.
const runKitStateDir = "${FACTORY_STATE_DIR:-$HOME/.factory/state}"

// cellWidth bounds a table cell; longer text is cut with "…".
const cellWidth = 60

// stateEnv is what factory state takes from the process: the environment, the home dir and
// the clock (for --since and reset's timestamp).
type stateEnv struct {
	getenv func(string) string
	home   func() (string, error)
	now    func() time.Time
}

func stateCommand() command {
	return command{
		name:    "state",
		summary: "read the dispatcher's state (a state dir or a --state-url), without the server: tasks, runs, work items, events, the journal",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runState(ctx, args, stateEnv{getenv: os.Getenv, home: os.UserHomeDir, now: time.Now}, stdout, stderr)
		},
	}
}

// stateSub is one factory state subcommand.
type stateSub struct {
	name, args, summary string
	run                 func(ctx context.Context, r *stateRun, args []string) int
}

func stateSubs() []stateSub {
	return []stateSub{
		{"tasks", "[--phase P] [--spec S] [--workitem W] [--since 24h]", "the tasks: id, spec, phase, reason, work item, created, last run", stateTasks},
		{"task", "TASK", "a task: its spec, status, runs with their results, and the decision that dispatched it", stateTask},
		{"run", "RUN [--transcript]", "a run: its Result, progress and usage; --transcript prints the transcript (gunzipped)", stateRunCmd},
		{"workitem", "WORKITEM", "a work item: links, state, counters, decisions and tasks", stateWorkItem},
		{"events", "[--since 1h] [--type T] [--workitem W]", "the events: time, type, source, object, work item, processed", stateEvents},
		{"log", "[--since 10m]", "the journal or the log (the changes since the snapshot) as a timeline", stateLog},
		{"reset", "--yes", "move the state dir aside to <dir>.<UTC time>, for a clean start: open items restart with no state and no run counters (not while a dispatcher has it open; not for --state-url)", stateReset},
	}
}

func stateUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: factory state [--state-dir DIR | --state-url URL] [--json] <command> [flags] [id]")
	fmt.Fprintln(w, "\nReads the dispatcher's state directly: no server, no API, no token. Read-only, it")
	fmt.Fprintln(w, "works while factory serve runs, on a copy of the dir, and on the bucket of a --state-url")
	fmt.Fprintln(w, "(gs://, s3:// or file://; its credentials come from the environment, as serve's do).")
	fmt.Fprintf(w, "--state-dir defaults to $%s, else ~/.factory/state (deploy/poc/run.sh: %s).\n", envStateDir, runKitStateDir)
	fmt.Fprintln(w, "\ncommands:")
	for _, s := range stateSubs() {
		fmt.Fprintf(w, "  %-9s %s\n", s.name, s.summary)
	}
}

// stateRun is one factory state invocation.
type stateRun struct {
	env            stateEnv
	stdout, stderr io.Writer
	name           string // "factory state tasks"
	dirFlag        string
	urlFlag        string
	json           bool
	dir, dirFrom   string // the state dir (or the state URL's scheme), and where it came from
	view           stateView
	// bucket is the --state-url's bucket: open while the subcommand reads through the view
	// (a run's transcript), closed by runState once it returns.
	bucket objectstore.Bucket
}

// close releases what open opened: the --state-url's bucket (the GCS and S3 clients hold
// connections, and a closed client must not be used again).
func (r *stateRun) close() {
	if r.bucket == nil {
		return
	}
	if err := closeBucket(r.bucket); err != nil {
		fmt.Fprintf(r.stderr, "%s: warning: %v\n", r.name, err)
	}
	r.bucket = nil
}

// stateView is the state factory state reads: a state dir's (filestore.View) or a bucket's
// (objectstore.View).
type stateView interface {
	Get(ctx context.Context, k store.Key, out store.Object) error
	List(ctx context.Context, kind string, opts store.ListOptions, out any) (store.ListMeta, error)
	// Journal is the changes since the snapshot, in version order; Version the newest.
	Journal() []filestore.JournalEntry
	Version() uint64
	// LogName names Journal in messages: "journal" or "log".
	LogName() string
	// Transcript is a run's transcript blob as stored (gzip'd).
	Transcript(ctx context.Context, run string) ([]byte, error)
	Warnings() []string
}

// dirView is a state dir's view.
type dirView struct{ *filestore.View }

func (v dirView) Version() uint64 { return v.Loaded().Version }
func (dirView) LogName() string   { return "journal" }
func (v dirView) Transcript(_ context.Context, run string) ([]byte, error) {
	return v.View.Transcript(run)
}

// bucketView is a --state-url's view.
type bucketView struct{ *objectstore.View }

func (v bucketView) Version() uint64 { return v.Loaded().Version }
func (bucketView) LogName() string   { return "log" }
func (v bucketView) Journal() []filestore.JournalEntry {
	log := v.Log()
	out := make([]filestore.JournalEntry, len(log))
	for i, e := range log {
		out[i] = filestore.JournalEntry{Version: e.Version, At: e.At, Op: e.Op, Kind: e.Kind, Name: e.Name, Obj: e.Obj}
	}
	return out
}

func runState(ctx context.Context, args []string, env stateEnv, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("factory state", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dir := fl.String("state-dir", "", "the dispatcher's state dir (default $"+envStateDir+", else ~/.factory/state)")
	stateURL := fl.String("state-url", "", "the dispatchers' shared state: gs://bucket/prefix, s3://bucket/prefix or file:///dir (instead of --state-dir)")
	asJSON := fl.Bool("json", false, "print the objects as JSON instead of tables")
	fl.Usage = func() {
		stateUsage(stderr)
		fmt.Fprintln(stderr)
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if fl.NArg() == 0 {
		stateUsage(stderr)
		return exitUsage
	}
	for _, s := range stateSubs() {
		if s.name == fl.Arg(0) {
			r := &stateRun{env: env, stdout: stdout, stderr: stderr, name: "factory state " + s.name, dirFlag: *dir, urlFlag: *stateURL, json: *asJSON}
			defer r.close()
			return s.run(ctx, r, fl.Args()[1:])
		}
	}
	fmt.Fprintf(stderr, "factory state: unknown command %q\n", fl.Arg(0))
	stateUsage(stderr)
	return exitUsage
}

// flags returns the subcommand's flag set, with --state-dir and --json (when withJSON).
func (r *stateRun) flags(args string, withJSON bool) *flag.FlagSet {
	fl := flag.NewFlagSet(r.name, flag.ContinueOnError)
	fl.SetOutput(r.stderr)
	fl.StringVar(&r.dirFlag, "state-dir", r.dirFlag, "the dispatcher's state dir (default $"+envStateDir+", else ~/.factory/state)")
	fl.StringVar(&r.urlFlag, "state-url", r.urlFlag, "the dispatchers' shared state: gs://bucket/prefix, s3://bucket/prefix or file:///dir (instead of --state-dir)")
	if withJSON {
		fl.BoolVar(&r.json, "json", r.json, "print the objects as JSON instead of tables")
	}
	fl.Usage = func() {
		fmt.Fprintf(r.stderr, "usage: %s %s [--state-dir DIR | --state-url URL]", r.name, args)
		if withJSON {
			fmt.Fprint(r.stderr, " [--json]")
		}
		fmt.Fprintln(r.stderr)
		fl.PrintDefaults()
	}
	return fl
}

// parse parses args, flags and positional arguments in any order, and checks that there are
// exactly want positional ones. code is the exit code when ok is false.
func (r *stateRun) parse(fl *flag.FlagSet, args []string, want int) (pos []string, code int, ok bool) {
	for {
		if err := fl.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, 0, false
			}
			return nil, exitUsage, false
		}
		args = fl.Args()
		if len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	if len(pos) != want {
		if want == 0 {
			fmt.Fprintf(r.stderr, "%s: unexpected arguments %v\n", r.name, pos)
		} else {
			fmt.Fprintf(r.stderr, "%s: want %d argument(s), got %v\n", r.name, want, pos)
		}
		fl.Usage()
		return nil, exitUsage, false
	}
	return pos, 0, true
}

// resolveDir sets the state dir: --state-dir, else $FACTORY_STATE_DIR, else ~/.factory/state.
func (r *stateRun) resolveDir() error {
	switch {
	case r.dirFlag != "":
		r.dir, r.dirFrom = r.dirFlag, "--state-dir"
	case r.env.getenv(envStateDir) != "":
		r.dir, r.dirFrom = r.env.getenv(envStateDir), "$"+envStateDir
	default:
		home, err := r.env.home()
		if err != nil {
			return fmt.Errorf("no --state-dir, no $%s, and no home dir for the default: %w", envStateDir, err)
		}
		r.dir, r.dirFrom = filepath.Join(home, ".factory", "state"), "the default"
	}
	return nil
}

// noStateDir says that there is no state dir where r looked, and how to point at one.
func (r *stateRun) noStateDir() int {
	fmt.Fprintf(r.stderr, "%s: no state dir at %s (%s): pass --state-dir DIR or set %s to the dispatcher's (deploy/poc/run.sh uses %s)\n",
		r.name, r.dir, r.dirFrom, envStateDir, runKitStateDir)
	return 1
}

// open reads the state dir, or the --state-url's bucket; code is the exit code when it can't.
func (r *stateRun) open(ctx context.Context) (code int, ok bool) {
	if r.urlFlag != "" {
		return r.openURL(ctx)
	}
	if err := r.resolveDir(); err != nil {
		fmt.Fprintf(r.stderr, "%s: %v\n", r.name, err)
		return 1, false
	}
	v, err := filestore.Read(r.dir, store.Options{Logger: slog.New(slog.DiscardHandler)}, fileStoreKinds()...)
	if errors.Is(err, os.ErrNotExist) {
		return r.noStateDir(), false
	}
	if err != nil {
		fmt.Fprintf(r.stderr, "%s: read %s: %v\n", r.name, r.dir, err)
		return 1, false
	}
	for _, w := range v.Warnings() {
		fmt.Fprintf(r.stderr, "%s: warning: %s\n", r.name, w)
	}
	r.view = dirView{v}
	return 0, true
}

// openURL reads the --state-url's bucket, once and read-only: it writes nothing and takes no
// lock, so it works while dispatchers run on it. Its errors never print the URL.
func (r *stateRun) openURL(ctx context.Context) (code int, ok bool) {
	if r.dirFlag != "" {
		fmt.Fprintf(r.stderr, "%s: --state-dir and --state-url are exclusive\n", r.name)
		return exitUsage, false
	}
	r.dir, r.dirFrom = "the "+stateURLScheme(r.urlFlag)+" state", "--state-url"
	ctx, cancel := context.WithTimeout(ctx, stateReadTimeout)
	defer cancel()
	b, err := objectstore.ParseURL(ctx, r.urlFlag)
	if err != nil {
		fmt.Fprintf(r.stderr, "%s: --state-url: %v\n", r.name, err)
		return 1, false
	}
	r.bucket = b // runState closes it once the subcommand is done
	v, err := objectstore.Read(ctx, b, store.Options{Logger: slog.New(slog.DiscardHandler)}, objectStoreKinds()...)
	if err != nil {
		fmt.Fprintf(r.stderr, "%s: read %s: %v\n", r.name, r.dir, err)
		return 1, false
	}
	for _, w := range v.Warnings() {
		fmt.Fprintf(r.stderr, "%s: warning: %s\n", r.name, w)
	}
	r.view = bucketView{v}
	return 0, true
}

// writeJSON prints v as indented JSON.
func (r *stateRun) writeJSON(v any) int {
	enc := json.NewEncoder(r.stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(r.stderr, "%s: %v\n", r.name, err)
		return 1
	}
	return 0
}

// fail prints err and returns 1.
func (r *stateRun) fail(err error) int {
	fmt.Fprintf(r.stderr, "%s: %v\n", r.name, err)
	return 1
}

// notFound says that id isn't a kind in the state dir, naming the ids with the longest
// common prefix with it.
func (r *stateRun) notFound(kind, id string, ids []string) int {
	best, near := 0, []string(nil)
	for _, other := range ids {
		n := commonPrefix(id, other)
		switch {
		case n == 0 || n < best:
		case n > best:
			best, near = n, []string{other}
		default:
			near = append(near, other)
		}
	}
	msg := fmt.Sprintf("%s: %s %s not found in %s", r.name, kind, id, r.dir)
	switch {
	case len(ids) == 0:
		msg += fmt.Sprintf(" (it holds no %s)", kind)
	case len(near) == 0:
		msg += fmt.Sprintf(" (no %s id shares a prefix with it; it holds %d)", kind, len(ids))
	default:
		slices.Sort(near)
		more := ""
		if len(near) > 5 {
			more = fmt.Sprintf(" and %d more", len(near)-5)
			near = near[:5]
		}
		msg += fmt.Sprintf("; nearest by prefix: %s%s", strings.Join(near, ", "), more)
	}
	fmt.Fprintln(r.stderr, msg)
	return 1
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// since parses a --since duration into the earliest time it keeps (zero for "").
func (r *stateRun) since(flagValue string) (time.Time, error) {
	if flagValue == "" {
		return time.Time{}, nil
	}
	d, err := v1alpha1.Duration(flagValue).Parse()
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("--since %q: want a positive duration such as 30m, 24h or 7d", flagValue)
	}
	return r.env.now().Add(-d), nil
}

// listAll lists every object of kind.
func listAll[T any](ctx context.Context, v stateView, kind string) ([]T, error) {
	var out []T
	if _, err := v.List(ctx, kind, store.ListOptions{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- formatting --------------------------------------------------------------------------------

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

// newIndentedTable is a table under a section heading: each line indented by two spaces.
func newIndentedTable(w io.Writer) *tabwriter.Writer {
	return newTable(&indentWriter{w: w, prefix: "  ", bol: true})
}

// indentWriter writes prefix at the start of every line.
type indentWriter struct {
	w      io.Writer
	prefix string
	bol    bool // at the beginning of a line
}

func (iw *indentWriter) Write(p []byte) (int, error) {
	var out []byte
	for _, b := range p {
		if iw.bol {
			out = append(out, iw.prefix...)
		}
		out = append(out, b)
		iw.bol = b == '\n'
	}
	if _, err := iw.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// row writes one table row of cells.
func row(w io.Writer, cells ...string) {
	for i, c := range cells {
		cells[i] = cell(c)
	}
	fmt.Fprintln(w, strings.Join(cells, "\t"))
}

// ts formats a time for the tables: UTC, to the second; "-" when unset.
func ts(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// cell is s on one line for a table: control characters (untrusted provider and agent text
// may hold terminal escapes) and runs of white space become one space, and it is cut at
// cellWidth. Empty is "-".
func cell(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if s == "" {
		return "-"
	}
	if utf8.RuneCountInString(s) > cellWidth {
		s = string([]rune(s)[:cellWidth-1]) + "…"
	}
	return s
}

// multiline is s for a multi-line field: control characters other than newlines and tabs are
// dropped, and each line after the first is indented by indent.
func multiline(s, indent string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, strings.TrimRight(s, "\n"))
	return strings.ReplaceAll(s, "\n", "\n"+indent)
}

// oneLineJSON is v as one line of JSON (map keys sorted).
func oneLineJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// field prints "  name  value" for a non-empty value, padded to the block's width.
func field(w io.Writer, name, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(w, "  %-11s %s\n", name, multiline(value, strings.Repeat(" ", 14)))
}

// ---- reset -------------------------------------------------------------------------------------

func stateReset(_ context.Context, r *stateRun, args []string) int {
	fl := r.flags("--yes", false)
	yes := fl.Bool("yes", false, "move the state dir aside (required)")
	if _, code, ok := r.parse(fl, args, 0); !ok {
		return code
	}
	if r.urlFlag != "" {
		fmt.Fprintf(r.stderr, "%s: reset moves a state dir aside; a --state-url's state is reset by stopping every dispatcher on it and deleting the objects under its prefix (%s)\n",
			r.name, resetLoss)
		return exitUsage
	}
	if err := r.resolveDir(); err != nil {
		return r.fail(err)
	}
	dst := r.dir + "." + r.env.now().UTC().Format("20060102T150405Z")
	if !*yes {
		fmt.Fprintf(r.stderr, "%s: this moves %s aside to %s, and the next factory serve starts with an empty state: %s; pass --yes to do it\n",
			r.name, r.dir, dst, resetLoss)
		return exitUsage
	}
	err := filestore.MoveAside(r.dir, dst)
	switch {
	case errors.Is(err, filestore.ErrLocked):
		fmt.Fprintf(r.stderr, "%s: a dispatcher has %s open (%s is locked): stop it first (deploy/poc/README.md §11), then run this again\n",
			r.name, r.dir, filepath.Base(filestore.LockPath(r.dir)))
		return 1
	case errors.Is(err, os.ErrNotExist):
		return r.noStateDir()
	case err != nil:
		return r.fail(err)
	}
	fmt.Fprintf(r.stdout, "moved %s to %s\n", r.dir, dst)
	fmt.Fprintln(r.stdout, "the next factory serve on it starts with an empty state: "+resetLoss)
	fmt.Fprintln(r.stdout, resetRecovery)
	return 0
}

// What a reset loses, and how to get back what matters (P3 Decision 7: nothing comes back
// from GitHub).
const (
	resetLoss     = "open work items restart with no state and no run counters (the per-item run cap included), and no stored event survives to replay"
	resetRecovery = "to recover: redeliver webhooks from the GitHub App's delivery log (GitHub keeps about 3 days), reopen issues, or re-apply labels"
)
