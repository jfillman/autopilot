package backends

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is the little of Python's repr and str that the Python read adapters put into text the agent sees
// ("{app!r} is not an Application name", "{service!r} is not an application API this deployment knows"), so the text a
// Denied carries is the Python one for any argument, not only a string.

// pyRepr is repr(v) for a decoded JSON value. A JSON object is shown with its keys sorted: Go has lost the order the
// agent sent them in.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyStrRepr(x)
	case json.Number:
		return pyNumberRepr(string(x))
	case float64:
		return pyFloatRepr(x)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
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
			parts[i] = pyStrRepr(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStrRepr(fmt.Sprint(v))
}

// pyStr is str(v): a string as it is, anything else as its repr.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// pyStrRepr is repr(s) for a str: single quotes unless the text holds a ' and no ", the escapes Python writes, and the
// \x, \u and \U forms for what is not printable.
func pyStrRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			fmt.Fprintf(&b, "\\x%02x", s[i])
			i++
			continue
		}
		i += n
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == ' ' || (r < 0x7f && unicode.IsPrint(r)):
			b.WriteRune(r)
		case r < 0x100 && !unicode.IsPrint(r):
			fmt.Fprintf(&b, "\\x%02x", r)
		case !unicode.IsPrint(r):
			if r < 0x10000 {
				fmt.Fprintf(&b, "\\u%04x", r)
			} else {
				fmt.Fprintf(&b, "\\U%08x", r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyNumberRepr is repr of a JSON number: an integer literal is a Python int, anything else a float.
func pyNumberRepr(lit string) string {
	if !strings.ContainsAny(lit, ".eE") {
		if n, err := strconv.ParseInt(lit, 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
		return strings.TrimPrefix(lit, "+")
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return lit
	}
	return pyFloatRepr(f)
}

// pyFloatRepr is repr(float): the shortest digits that read back the same, in exponent form when the decimal point
// falls beyond 16 digits or before the fourth place after it.
func pyFloatRepr(f float64) string {
	switch {
	case f != f:
		return "nan"
	case f > 1.7976931348623157e308:
		return "inf"
	case f < -1.7976931348623157e308:
		return "-inf"
	case f == 0:
		if 1/f < 0 {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde+XX
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, expo, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp10, _ := strconv.Atoi(expo)
	decpt := exp10 + 1
	if decpt > 16 || decpt <= -4 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		x := decpt - 1
		es := "+"
		if x < 0 {
			es, x = "-", -x
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, es, x)
	}
	switch {
	case decpt <= 0:
		return sign + "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	}
	return sign + digits[:decpt] + "." + digits[decpt:]
}

// pyStrip is str.strip(): the white space Python knows, which includes the four information separators U+001C to U+001F.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// pyTruthy is bool(v) for a decoded JSON value.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return err != nil || f != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// maxEcho is how much of an argument a refusal message repeats back to the agent.
const maxEcho = 100

// echo is repr(v) for a refusal message, cut to maxEcho characters (Python repeats the argument whole). An ordinary
// argument is unchanged.
func echo(v any) string {
	if s, ok := v.(string); ok {
		if r := []rune(s); len(r) > maxEcho {
			return pyStrRepr(string(r[:maxEcho])) + "..."
		}
		return pyStrRepr(s)
	}
	r := []rune(pyRepr(v))
	if len(r) > maxEcho {
		return string(r[:maxEcho]) + "..."
	}
	return string(r)
}
