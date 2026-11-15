package airframe

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
)

// The helpers the sidecar contract shares with airframe-verify (src/autopilot/vendor/airframe_sidecars.py): dig, render and
// when_matches. They read an XR that came from the cluster, so everything an XR holds is data: nothing here panics on a value of
// the wrong type, and what a template takes from an XR is bounded.

// maxRendered is the longest value a template takes from an XR (Python has no bound): a tenant's XR can name a long string, and
// a rendered name goes into a path and a message. A longer value is not published, as one that is not there.
const maxRendered = 1024

// Unresolved is a template key the XR does not publish (airframe_sidecars.Unresolved).
type Unresolved struct{ Key string }

func (u *Unresolved) Error() string { return pyrepr.Str(u.Key) }

var templateRE = regexp.MustCompile(`\{([a-zA-Z][A-Za-z0-9_.]*)\}`)

// norm is a decoded JSON value with its numbers as int64 or float64 (an upstream's answer keeps json.Number).
func stNorm(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, err := x.Float64()
		if err != nil || math.IsInf(f, 0) {
			return nil
		}
		return f
	case int:
		return int64(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = stNorm(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = stNorm(e)
		}
		return out
	case *definitions.OMap:
		return stNorm(definitions.Plain(x))
	}
	return v
}

func stAsMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func stAsList(v any) []any {
	l, _ := v.([]any)
	return l
}

// truthy is Python's bool(v) for a normalised value.
func stTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// or is `a or b`.
func stOr(a, b any) any {
	if stTruthy(a) {
		return a
	}
	return b
}

// num is v as a number for an ordering comparison (a boolean or a string is not one here: Python would raise).
func stNum(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// pyEq is a == b for normalised values (and contract values, which may be an OMap): a number equals a number of the same value,
// and a boolean is 0 or 1 as in Python.
func stPyEq(a, b any) bool {
	a, b = stNorm(a), stNorm(b)
	fa, aok := stNumOrBool(a)
	fb, bok := stNumOrBool(b)
	if aok || bok {
		return aok && bok && fa == fb
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !stPyEq(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, e := range x {
			f, has := y[k]
			if !has || !stPyEq(e, f) {
				return false
			}
		}
		return true
	}
	return false
}

func stNumOrBool(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return stNum(v)
}

// pyRepr is repr(v) for a normalised value; an object is shown with its keys sorted (the order an XR's keys were written in is
// not kept).
func stPyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyrepr.Str(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return pyrepr.Float(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = stPyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyrepr.Str(k) + ": " + stPyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *definitions.OMap:
		return pyrepr.Value(x)
	}
	return fmt.Sprint(v)
}

// pyStr is str(v).
func stPyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return stPyRepr(v)
}

// Dig is airframe_sidecars.dig: `a.b.c` into nested objects; `a.list[].b` collects b from every element of list (a list).
func Dig(obj any, path string) any {
	cur := []any{obj}
	multi := false
	for _, part := range strings.Split(path, ".") {
		var next []any
		isList := strings.HasSuffix(part, "[]")
		key := strings.TrimSuffix(part, "[]")
		for _, c := range cur {
			m := stAsMap(c)
			if m == nil {
				continue
			}
			val, ok := m[key]
			if !ok {
				continue
			}
			if isList {
				multi = true
				next = append(next, stAsList(val)...)
			} else {
				next = append(next, val)
			}
		}
		cur = next
	}
	if multi {
		if cur == nil {
			return []any{}
		}
		return cur
	}
	if len(cur) > 0 {
		return cur[0]
	}
	return nil
}

// Render is airframe_sidecars.render: {name}, {namespace}, {spec.x} and {status.x} filled from an XR; a template that is not a
// string passes through. A key the XR does not publish (or one that is an object or a list, or is too long) is an *Unresolved.
func Render(tpl any, xr map[string]any) (any, error) {
	s, ok := tpl.(string)
	if !ok {
		return tpl, nil
	}
	md := stAsMap(xr["metadata"])
	var failed *Unresolved
	out := templateRE.ReplaceAllStringFunc(s, func(m string) string {
		if failed != nil {
			return ""
		}
		key := m[1 : len(m)-1]
		var val string
		switch {
		case key == "name" || key == "namespace":
			v, has := md[key]
			if !has {
				v = ""
			}
			val = stPyStr(v)
		case strings.HasPrefix(key, "spec.") || strings.HasPrefix(key, "status."):
			v := Dig(xr, key)
			switch v.(type) {
			case nil, map[string]any, []any:
				failed = &Unresolved{Key: key}
				return ""
			}
			val = stPyStr(v)
		default:
			failed = &Unresolved{Key: key}
			return ""
		}
		if len(val) > maxRendered {
			failed = &Unresolved{Key: key}
			return ""
		}
		return val
	})
	if failed != nil {
		return nil, failed
	}
	return out, nil
}

// WhenMatches is airframe_sidecars.when_matches; when is a contract's `when` object (nil or empty: always).
func WhenMatches(when any, xr map[string]any) bool {
	w, _ := when.(*definitions.OMap)
	if w == nil || len(w.Keys) == 0 {
		return true
	}
	field, _ := w.M["field"].(string)
	v := Dig(xr, field)
	if eq, ok := w.M["equals"]; ok {
		return stPyEq(v, eq)
	}
	for _, e := range stAsList(w.M["in"]) {
		if stPyEq(v, e) {
			return true
		}
	}
	return false
}
