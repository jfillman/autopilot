package airframe

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// Bounds of an argument the planner or a validator reads: an agent chose it.
const (
	maxArgDepth = 64
	maxArgNodes = 20000
)

// FromArgs turns a handler's arguments (map[string]any, []any, string, bool, nil, json.Number, float64, int) into the ordered value
// model: *definitions.OMap with its keys sorted (a plain map has no order), int64, float64, string, bool, nil, []any. A number
// written as an integer within 2^53 is an int64, any other a float64; NaN and infinity are refused, as are a document nested more
// than 64 deep or with more than 20,000 values.
func FromArgs(v any) (any, error) {
	n := 0
	return fromArgs(v, 0, &n)
}

func fromArgs(v any, depth int, n *int) (any, error) {
	if *n++; *n > maxArgNodes {
		return nil, errors.New("the document has more than 20000 values")
	}
	if depth > maxArgDepth {
		return nil, errors.New("the document is nested more than 64 deep")
	}
	switch x := v.(type) {
	case nil, bool, string:
		return x, nil
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, errors.New("a number is not finite")
		}
		if x == math.Trunc(x) && math.Abs(x) <= 1<<53 {
			// encoding/json gave a whole number as a float64: it was written as an integer unless it had a point, which is lost.
			return int64(x), nil
		}
		return x, nil
	case json.Number:
		s := string(x)
		if !strings.ContainsAny(s, ".eE") {
			if i, err := x.Int64(); err == nil && i >= -(1<<53) && i <= 1<<53 {
				return i, nil
			}
		}
		f, err := x.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%q is not a number this planner reads", s)
		}
		return f, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			c, err := fromArgs(e, depth+1, n)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := &definitions.OMap{M: make(map[string]any, len(x))}
		for _, k := range keys {
			c, err := fromArgs(x[k], depth+1, n)
			if err != nil {
				return nil, err
			}
			o.Set(k, c)
		}
		return o, nil
	case *definitions.OMap:
		o := &definitions.OMap{M: make(map[string]any, len(x.Keys))}
		for _, k := range x.Keys {
			c, err := fromArgs(x.M[k], depth+1, n)
			if err != nil {
				return nil, err
			}
			o.Set(k, c)
		}
		return o, nil
	}
	return nil, fmt.Errorf("a value of type %T is not JSON", v)
}
