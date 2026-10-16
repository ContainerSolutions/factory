package cel

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// yamlAdapter converts activation values (YAML/JSON trees) to CEL values. Catalog literals and
// stored state come through encoding/json, so every number is a float64: a whole float64 (or
// json.Number) becomes a CEL int, so `item.state.stackNext + 1` works on state written as
// `stackNext: 0`. Other floats stay doubles. Maps and lists are wrapped lazily with this
// adapter, so nested numbers are converted the same way without copying the tree.
type yamlAdapter struct{ base types.Adapter }

func (a yamlAdapter) NativeToValue(v any) ref.Val {
	switch x := v.(type) {
	case ref.Val:
		return x
	case float64:
		return number(x)
	case float32:
		return number(float64(x))
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return types.Int(i)
		}
		f, err := x.Float64()
		if err != nil {
			return types.NewErr("invalid number %q", string(x))
		}
		return number(f)
	case map[string]any:
		return types.NewStringInterfaceMap(a, x)
	case []any:
		return types.NewDynamicList(a, x)
	case []byte, time.Time, time.Duration:
		return a.base.NativeToValue(v)
	}
	if v != nil {
		switch reflect.TypeOf(v).Kind() {
		case reflect.Map:
			return types.NewDynamicMap(a, v)
		case reflect.Slice, reflect.Array:
			return types.NewDynamicList(a, v)
		}
	}
	return a.base.NativeToValue(v)
}

// number is a CEL int when f is a whole number in int64 range, a double otherwise.
func number(f float64) ref.Val {
	if f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63 {
		return types.Int(int64(f))
	}
	return types.Double(f)
}

// toNative converts a CEL result to a YAML value: map[string]any, []any, string, bool, int64,
// uint64, float64, nil, []byte, time.Time or time.Duration.
func toNative(v ref.Val) (any, error) {
	switch x := v.(type) {
	case *types.Err:
		return nil, x
	case types.Bool:
		return bool(x), nil
	case types.Int:
		return int64(x), nil
	case types.Uint:
		return uint64(x), nil
	case types.Double:
		return float64(x), nil
	case types.String:
		return string(x), nil
	case types.Bytes:
		return []byte(x), nil
	case types.Null:
		return nil, nil
	case types.Timestamp:
		return x.Time, nil
	case types.Duration:
		return x.Duration, nil
	case traits.Mapper:
		out := map[string]any{}
		for it := x.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			ks, ok := k.(types.String)
			if !ok {
				return nil, fmt.Errorf("%w: map key %v is %s, want string", ErrType, k, k.Type().TypeName())
			}
			ev, err := toNative(x.Get(k))
			if err != nil {
				return nil, err
			}
			out[string(ks)] = ev
		}
		return out, nil
	case traits.Lister:
		n, ok := x.Size().(types.Int)
		if !ok {
			return nil, fmt.Errorf("%w: list without a size", ErrType)
		}
		out := make([]any, 0, int(n))
		for it := x.Iterator(); it.HasNext() == types.True; {
			ev, err := toNative(it.Next())
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: %s is not a YAML value", ErrType, v.Type().TypeName())
	}
}
