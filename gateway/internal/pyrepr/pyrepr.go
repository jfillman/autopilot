// Package pyrepr is the little of Python's repr that error text copied from the Python gateway needs: the definition
// schema's messages quote the offending value as Python's jsonschema does ("'T3' is not one of ['T0', 'T1', 'T2']").
// It knows the value types the definition loader decodes: nil, bool, int64, float64, string, []any, and an ordered
// object (Ordered), shown in the order it was written, as Python's dict is.
package pyrepr

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ordered is an object that remembers the order its keys were written in.
type Ordered interface {
	OrderedKeys() []string
	OrderedGet(key string) (any, bool)
}

// Value is repr(v).
func Value(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return Str(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return Float(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = Value(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = Str(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case Ordered:
		keys := x.OrderedKeys()
		parts := make([]string, len(keys))
		for i, k := range keys {
			e, _ := x.OrderedGet(k)
			parts[i] = Str(k) + ": " + Value(e)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// Text is str(v): a string as it is, anything else as its repr.
func Text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return Value(v)
}

// Str is repr(s) for a str: single quotes unless the text holds a ' and no ", the escapes Python writes, and the \x, \u
// and \U forms for what is not printable.
func Str(s string) string {
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

// Float is repr(float): the shortest digits that read back the same, in exponent form when the decimal point falls beyond 16
// digits or before the fourth place after it.
func Float(f float64) string {
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
	e := strconv.FormatFloat(f, 'e', -1, 64)
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
