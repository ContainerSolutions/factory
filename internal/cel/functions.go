package cel

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"

	"github.com/compliance-framework/factory/internal/patterns"
)

// Item keys read by the item functions. The rule engine fills them from WorkItemView:
// counters from the run counts, runs from RunsBySpec.
const (
	itemCounters = "counters" // map: TaskSpec name → number of Runs
	itemRuns     = "runs"     // map: TaskSpec name → list of Run summaries, oldest first
)

// functions returns the custom functions of the environment (design LLD §Custom functions).
func functions(globs *globCache) []cel.EnvOption {
	itemType := cel.MapType(cel.StringType, cel.DynType)
	return []cel.EnvOption{
		cel.Function("count",
			cel.MemberOverload("item_count_string", []*cel.Type{itemType, cel.StringType}, cel.IntType,
				cel.BinaryBinding(itemCount))),
		cel.Function("lastRun",
			cel.MemberOverload("item_lastRun_string", []*cel.Type{itemType, cel.StringType}, cel.DynType,
				cel.BinaryBinding(itemLastRun))),
		cel.Function("hash",
			cel.Overload("hash_string", []*cel.Type{cel.StringType}, cel.IntType,
				cel.UnaryBinding(hashString))),
		cel.Function("semver.bump",
			cel.Overload("semver_bump_string_string", []*cel.Type{cel.StringType, cel.StringType}, cel.StringType,
				cel.BinaryBinding(semverBump))),
		cel.Function("glob",
			cel.Overload("glob_string_string", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType,
				cel.BinaryBinding(globs.match))),
	}
}

// lookup returns m[key], or nil when m is not a map or has no such key.
func lookup(m ref.Val, key ref.Val) ref.Val {
	mapper, ok := m.(traits.Mapper)
	if !ok {
		return nil
	}
	v, found := mapper.Find(key)
	if !found {
		return nil
	}
	return v
}

// itemCount is item.count(spec): item.counters[spec], 0 when absent.
func itemCount(item, spec ref.Val) ref.Val {
	if _, ok := item.(traits.Mapper); !ok {
		return types.NewErr("count: receiver is %s, want the item map", item.Type().TypeName())
	}
	v := lookup(lookup(item, types.String(itemCounters)), spec)
	switch n := v.(type) {
	case nil:
		return types.IntZero
	case types.Int:
		return n
	case types.Uint:
		return types.Int(n)
	case types.Double:
		return types.Int(n)
	default:
		return types.NewErr("count: item.counters[%v] is %s, want int", spec, v.Type().TypeName())
	}
}

// itemLastRun is item.lastRun(spec): the last element of item.runs[spec], null when absent.
func itemLastRun(item, spec ref.Val) ref.Val {
	if _, ok := item.(traits.Mapper); !ok {
		return types.NewErr("lastRun: receiver is %s, want the item map", item.Type().TypeName())
	}
	v := lookup(lookup(item, types.String(itemRuns)), spec)
	if v == nil {
		return types.NullValue
	}
	runs, ok := v.(traits.Lister)
	if !ok {
		return types.NewErr("lastRun: item.runs[%v] is %s, want a list", spec, v.Type().TypeName())
	}
	n, ok := runs.Size().(types.Int)
	if !ok || n == 0 {
		return types.NullValue
	}
	return runs.Get(n - 1)
}

// hashString is hash(s): FNV-1a 32-bit, as a non-negative int.
func hashString(s ref.Val) ref.Val {
	str, ok := s.(types.String)
	if !ok {
		return types.MaybeNoSuchOverloadErr(s)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(str))
	return types.Int(int64(h.Sum32()))
}

// semverBump is semver.bump(from, to): the most significant of major, minor and patch that
// differs, or "none" when the cores are equal (prerelease and build metadata are ignored).
// A downgrade reports the component that differs too.
func semverBump(a, b ref.Val) ref.Val {
	as, ok1 := a.(types.String)
	bs, ok2 := b.(types.String)
	if !ok1 || !ok2 {
		return types.MaybeNoSuchOverloadErr(a)
	}
	from, err := parseSemver(string(as))
	if err != nil {
		return types.NewErr("semver.bump: %v", err)
	}
	to, err := parseSemver(string(bs))
	if err != nil {
		return types.NewErr("semver.bump: %v", err)
	}
	for i, name := range [3]string{"major", "minor", "patch"} {
		if from[i] != to[i] {
			return types.String(name)
		}
	}
	return types.String("none")
}

// parseSemver parses MAJOR.MINOR.PATCH with an optional leading "v" and optional
// -prerelease / +build suffixes, which are dropped.
func parseSemver(s string) ([3]uint64, error) {
	var out [3]uint64
	core := strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("%q is not a semantic version (want MAJOR.MINOR.PATCH)", s)
	}
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return out, fmt.Errorf("%q is not a semantic version: bad number %q", s, p)
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return out, fmt.Errorf("%q is not a semantic version: bad number %q", s, p)
		}
		out[i] = n
	}
	return out, nil
}

// globCacheSize bounds the compiled patterns kept by one environment. Patterns are usually
// literals from the catalog; the cache is reset when a dynamic pattern overflows it.
const globCacheSize = 256

// globCache compiles glob patterns once per environment. Safe for concurrent use.
type globCache struct {
	mu sync.Mutex
	m  map[string]patterns.Pattern
}

func newGlobCache() *globCache { return &globCache{m: map[string]patterns.Pattern{}} }

// match is glob(pattern, s), with internal/patterns semantics (`*`, globs, `re:` regexes,
// exact strings).
func (c *globCache) match(pattern, s ref.Val) ref.Val {
	ps, ok1 := pattern.(types.String)
	ss, ok2 := s.(types.String)
	if !ok1 || !ok2 {
		return types.MaybeNoSuchOverloadErr(pattern)
	}
	p, err := c.get(string(ps))
	if err != nil {
		return types.NewErr("glob: %v", err)
	}
	return types.Bool(p.Match(string(ss)))
}

func (c *globCache) get(src string) (patterns.Pattern, error) {
	c.mu.Lock()
	p, ok := c.m[src]
	c.mu.Unlock()
	if ok {
		return p, nil
	}
	p, err := patterns.Compile(src)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.m) >= globCacheSize {
		c.m = map[string]patterns.Pattern{}
	}
	c.m[src] = p
	c.mu.Unlock()
	return p, nil
}
