package rules

import (
	"sort"
	"unicode/utf8"

	"github.com/compliance-framework/factory/internal/cel"
)

// Bounds of the where-inputs snapshot kept on matches and decisions.
const (
	snapshotDepth  = 4
	snapshotString = 256
	snapshotList   = 20
	snapshotKeys   = 50
)

// truncated marks what the snapshot left out.
const truncated = "…"

// whereInputs is the bounded snapshot of the variables the where reads ("why did this
// happen?"): nil for an empty where.
func whereInputs(where *cel.Program, act cel.Activation) map[string]any {
	if where == nil {
		return nil
	}
	vars := where.Variables()
	if len(vars) == 0 {
		return nil
	}
	all := map[string]any{
		cel.VarEvent: act.Event, cel.VarActor: act.Actor, cel.VarItem: act.Item, cel.VarRun: act.Run,
		cel.VarApproval: act.Approval, cel.VarProvider: act.Provider, cel.VarParams: act.Params,
		cel.VarNow: act.Now,
	}
	out := make(map[string]any, len(vars))
	for _, v := range vars {
		if val, ok := all[v]; ok {
			out[v] = bound(val, 0)
		}
	}
	return out
}

// bound copies v within the snapshot limits.
func bound(v any, depth int) any {
	switch x := v.(type) {
	case string:
		if len(x) <= snapshotString {
			return x
		}
		cut := snapshotString
		for cut > 0 && !utf8.RuneStart(x[cut]) {
			cut--
		}
		return x[:cut] + truncated
	case map[string]any:
		if x == nil {
			return x
		}
		if depth >= snapshotDepth {
			return truncated
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, min(len(keys), snapshotKeys+1))
		for i, k := range keys {
			if i == snapshotKeys {
				out[truncated] = len(keys) - snapshotKeys
				break
			}
			out[k] = bound(x[k], depth+1)
		}
		return out
	case []any:
		if x == nil {
			return x
		}
		if depth >= snapshotDepth {
			return truncated
		}
		n := min(len(x), snapshotList)
		out := make([]any, 0, n+1)
		for _, e := range x[:n] {
			out = append(out, bound(e, depth+1))
		}
		if len(x) > n {
			out = append(out, truncated)
		}
		return out
	default:
		return v
	}
}
