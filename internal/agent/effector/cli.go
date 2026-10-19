package effector

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// match finds the op whose words start args (the longest match) and returns it with the
// remaining arguments.
func match(args []string) (*Op, []string) {
	var best *Op
	for _, o := range ops {
		if len(args) < len(o.Words) || (best != nil && len(o.Words) <= len(best.Words)) {
			continue
		}
		ok := true
		for i, w := range o.Words {
			ok = ok && args[i] == w
		}
		if ok {
			best = o
		}
	}
	if best == nil {
		return nil, nil
	}
	return best, args[len(best.Words):]
}

// IsOp reports whether args name an Effector op (as opposed to the fx command's internal
// ones).
func IsOp(args []string) bool {
	o, _ := match(args)
	return o != nil
}

// Main runs `factory <op> [flags]` and returns its exit code (effector.md §Exit codes). The
// op's result is printed as JSON on stdout; a failure prints a message on stderr and
// {"error": …} on stdout.
func Main(ctx context.Context, e *Effector, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		w := stdout
		if len(args) == 0 {
			w = stderr
		}
		Usage(w)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	o, rest := match(args)
	if o == nil {
		fmt.Fprintf(stderr, "factory: unknown op %q\n\n", strings.Join(args, " "))
		Usage(stderr)
		return ExitUsage
	}
	raw, err := o.flags(rest, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", o.Command(), err)
		return ExitUsage
	}
	out, err := e.do(ctx, o, raw, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", o.Command(), err)
		writeJSON(stdout, map[string]any{"error": errorJSON(err)})
		return exitCode(err)
	}
	writeJSON(stdout, out)
	return ExitOK
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// errorJSON is an error as the JSON both front-ends report.
func errorJSON(err error) map[string]any {
	out := map[string]any{"message": err.Error(), "exitCode": exitCode(err)}
	var e *Error
	if errors.As(err, &e) {
		if e.Problem != nil {
			out["problem"] = e.Problem
		}
		for k, v := range e.Extra {
			out[k] = v
		}
	}
	return out
}

// flags parses an op's CLI flags into the raw arguments Op.parse takes.
func (o *Op) flags(argv []string, stderr io.Writer) (map[string]any, error) {
	fs := flag.NewFlagSet(o.Command(), flag.ContinueOnError)
	fs.SetOutput(stderr)
	raw := map[string]any{}
	for _, p := range o.Params {
		name := p.Name
		switch p.Kind {
		case Bool:
			fs.BoolFunc(p.CLIFlag(), p.Help, func(s string) error { raw[name] = s; return nil })
		case List:
			fs.Func(p.CLIFlag(), p.Help+" (repeatable, or comma-separated)", func(s string) error {
				l, _ := raw[name].([]any)
				raw[name] = append(l, s)
				return nil
			})
		case Text, JSON:
			fs.Func(p.CLIFlag(), p.Help, func(s string) error {
				if p.Kind == JSON {
					raw[name] = json.RawMessage(s)
				} else {
					raw[name] = s
				}
				return nil
			})
			fs.Func(p.CLIFlag()+"-file", "read "+p.Name+" from a file (- is stdin)", func(s string) error {
				raw[fileKey(name)] = s
				return nil
			})
		default:
			fs.Func(p.CLIFlag(), p.Help, func(s string) error { raw[name] = s; return nil })
		}
	}
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s [flags]\n\n%s\n\nflags:\n", o.Command(), o.Help)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q (every value is a flag)", fs.Arg(0))
	}
	return raw, nil
}

// Usage prints the op list.
func Usage(w io.Writer) {
	fmt.Fprintln(w, "usage: factory <op> [flags]   (factory-agent fx <op>; `factory <op> -h` for its flags)")
	fmt.Fprintln(w, "\nops:")
	for _, o := range ops {
		fmt.Fprintf(w, "  %-22s %s\n", strings.Join(o.Words, " "), o.Help)
	}
	fmt.Fprintln(w, "  mcp                    the same ops as a stdio MCP server")
	fmt.Fprintln(w, "\nexit codes: 0 ok, 2 usage, 3 not allowed (local check), 4 refused or failed, 5 git failure")
}
