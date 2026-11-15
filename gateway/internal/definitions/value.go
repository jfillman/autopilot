package definitions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// This file reads a definition document strictly. A definition is attacker-influenced input (a pin names a file in an app's own
// repository), and the Python loader reads it with yaml.safe_load, which is YAML 1.1 and lenient: a duplicate key is silently the
// last one, `yes` is a boolean, 0755 is an octal number, 1:30 is the integer 90, an alias can expand a few lines into a billion.
// What the schema then validates is what that reading produced, not what the reviewer read. Here a document is accepted only when
// it reads the same under YAML 1.1 and 1.2 and has nothing in it that needs the rules to be known: see (*reader).scalar. Every
// refusal is a StrictError, and the reason a pin shows is "<file> is not acceptable: <why>".

// Limits of one definition file and of the set. A definition the schema accepts is a few KiB (the instructions are at most 1500
// characters); these are generous and exist so a hostile file costs a bounded amount of work.
const (
	MaxFileBytes = 64 << 10 // one pinned file
	MaxNodes     = 5000     // YAML nodes in one file
	MaxDepth     = 24       // nesting in one file
	MaxIntDigits = 16       // an integer up to 2^53 reads the same as a float and a JSON number everywhere
	MaxPins      = 256      // Agent XRs one poll accepts: the core's own limit on a published set
)

// OMap is an object that remembers the order its keys were written in (Python's dict): the schema's messages list values in
// that order. Values are nil, bool, int64, float64, string, []any and *OMap.
type OMap struct {
	Keys []string
	M    map[string]any
}

func newOMap() *OMap { return &OMap{M: map[string]any{}} }

// Get is m[k].
func (o *OMap) Get(k string) (any, bool) { v, ok := o.M[k]; return v, ok }

// Set adds k, or replaces it keeping its place.
func (o *OMap) Set(k string, v any) {
	if _, ok := o.M[k]; !ok {
		o.Keys = append(o.Keys, k)
	}
	o.M[k] = v
}

// OrderedKeys and OrderedGet make an OMap a pyrepr.Ordered.
func (o *OMap) OrderedKeys() []string           { return o.Keys }
func (o *OMap) OrderedGet(k string) (any, bool) { return o.Get(k) }

// Plain is v as ordinary Go maps and slices (what is published, marshalled and compared).
func Plain(v any) any {
	switch x := v.(type) {
	case *OMap:
		m := make(map[string]any, len(x.Keys))
		for _, k := range x.Keys {
			m[k] = Plain(x.M[k])
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Plain(e)
		}
		return out
	}
	return v
}

// FromPlain is Plain's inverse, with the keys of each object in sorted order (a document that arrived as a plain map has none).
func FromPlain(v any) any {
	switch x := v.(type) {
	case map[string]any:
		o := newOMap()
		for _, k := range sortedMapKeys(x) {
			o.Set(k, FromPlain(x[k]))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = FromPlain(e)
		}
		return out
	case int:
		return int64(x)
	case float64:
		// A document that went through encoding/json (a stored row) has lost the difference between 20 and 20.0.
		if x == float64(int64(x)) && x >= -(1<<53) && x <= 1<<53 {
			return int64(x)
		}
	}
	return v
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// StrictError is a document refused for how it is written, not for what the schema says of it.
type StrictError struct{ Msg string }

func (e *StrictError) Error() string { return e.Msg }

func strict(format string, a ...any) error { return &StrictError{fmt.Sprintf(format, a...)} }

// -- JSON ---------------------------------------------------------------------------------------------------------------------

// DecodeJSON reads one JSON value (a stored document, a candidate's) keeping the order of keys, refusing a repeated key, nesting
// beyond MaxDepth and a number that is not a whole number below 2^53 (a fraction is a float64, which no definition field takes).
func DecodeJSON(raw []byte) (any, error) {
	if len(raw) > MaxFileBytes*4 {
		return nil, strict("the document is larger than %d KiB", MaxFileBytes*4>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeJSONValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, strict("data after the JSON value")
	}
	return v, nil
}

func decodeJSONValue(dec *json.Decoder, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, strict("nested more than %d levels", MaxDepth)
	}
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch x := t.(type) {
	case json.Delim:
		if x == '{' {
			o := newOMap()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				if _, dup := o.M[k]; dup {
					return nil, strict("duplicate key %q", k)
				}
				v, err := decodeJSONValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		}
		out := []any{}
		for dec.More() {
			v, err := decodeJSONValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return out, nil
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			if n > 1<<53 || n < -(1<<53) {
				return nil, strict("the integer %s is larger than a definition needs", x)
			}
			return n, nil
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return nil, strict("the number %s is not one", x)
		}
		return f, nil
	}
	return t, nil // string, bool, nil
}

// -- YAML ---------------------------------------------------------------------------------------------------------------------

// ErrNotAMapping is a file that is YAML but not a mapping at its top (a scalar, a list, nothing at all).
var ErrNotAMapping = errors.New("not a YAML mapping")

// YAMLSyntaxError is a file the YAML parser could not read.
type YAMLSyntaxError struct{ First string }

func (e *YAMLSyntaxError) Error() string { return e.First }

// ParseYAML reads one pinned file: exactly one document, a mapping, read as strictly as value.go's header says. It returns
// ErrNotAMapping for any other top-level value, a *YAMLSyntaxError for what the parser refuses, and a *StrictError for what it
// reads but this loader does not accept.
func ParseYAML(src []byte) (*OMap, error) {
	if len(src) > MaxFileBytes {
		return nil, strict("the file is larger than %d KiB", MaxFileBytes>>10)
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrNotAMapping
		}
		return nil, &YAMLSyntaxError{firstLine(err.Error())}
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, &YAMLSyntaxError{firstLine(err.Error())}
		}
		return nil, &YAMLSyntaxError{"expected a single document in the stream"}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		return nil, ErrNotAMapping
	}
	r := &reader{}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		// A scalar or a list at the top is not a definition whatever it holds; still refuse what is in it that is unsafe to read.
		if _, err := r.value(top, 0); err != nil {
			return nil, err
		}
		return nil, ErrNotAMapping
	}
	v, err := r.value(top, 0)
	if err != nil {
		return nil, err
	}
	return v.(*OMap), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "yaml: ")
}

type reader struct{ nodes int }

func (r *reader) value(n *yaml.Node, depth int) (any, error) {
	if r.nodes++; r.nodes > MaxNodes {
		return nil, strict("more than %d YAML nodes", MaxNodes)
	}
	if depth > MaxDepth {
		return nil, strict("nested more than %d levels", MaxDepth)
	}
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return nil, strict("anchors and aliases (&x, *x) are not accepted: write the value out")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return nil, strict("an explicit tag (%s) is not accepted: write the value plainly", n.Tag)
	}
	switch n.Kind {
	case yaml.MappingNode:
		o := newOMap()
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, err := r.key(n.Content[i])
			if err != nil {
				return nil, err
			}
			if _, dup := o.M[k]; dup {
				return nil, strict("the key %q is written twice", k)
			}
			v, err := r.value(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			o.Set(k, v)
		}
		return o, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := r.value(c, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return r.scalar(n)
	}
	return nil, strict("a YAML construct this loader does not read")
}

func (r *reader) key(n *yaml.Node) (string, error) {
	if r.nodes++; r.nodes > MaxNodes {
		return "", strict("more than %d YAML nodes", MaxNodes)
	}
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return "", strict("anchors and aliases (&x, *x) are not accepted: write the value out")
	}
	if n.Kind != yaml.ScalarNode || n.Style&yaml.TaggedStyle != 0 {
		return "", strict("a mapping key must be a plain or quoted string")
	}
	v, err := r.scalar(n)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", strict("the key %q is not a string: quote it", n.Value)
	}
	if n.Style == 0 && s == "<<" {
		return "", strict("a merge key (<<) is not accepted")
	}
	return s, nil
}

var (
	plainInt    = regexp.MustCompile(`^[-+]?(0|[1-9][0-9]*)$`)
	simpleFloat = regexp.MustCompile(`^[-+]?(0|[1-9][0-9]*)\.[0-9]+$`)
	// What a YAML 1.1 reader (PyYAML) or a YAML 1.2 reader reads as something other than a string, apart from a plain decimal
	// integer: an integer written in another base or with underscores or colons, a float, an infinity, a timestamp.
	yaml11Int  = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	yaml11Flt  = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	yaml12Num  = regexp.MustCompile(`^(?:[-+]?[0-9]+|0o[0-7]+|0x[0-9a-fA-F]+|[-+]?(?:\.[0-9]+|[0-9]+(?:\.[0-9]*)?)(?:[eE][-+]?[0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	timestamp  = regexp.MustCompile(`^(?:[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}(?:[Tt]|[ \t]+)[0-9]{1,2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9]{1,2}(?::[0-9]{2})?))?)$`)
	yaml11Bool = map[string]bool{"yes": true, "Yes": true, "YES": true, "no": true, "No": true, "NO": true, "on": true, "On": true, "ON": true,
		"off": true, "Off": true, "OFF": true}
)

// scalar reads one scalar. A quoted or block scalar is a string. A plain one is null, true or false (the spellings both YAML 1.1
// and 1.2 give those meanings), a plain decimal integer, or a string; a plain scalar that either version reads as something else
// (yes, 0755, 0x14, 1_000, 1:30, 1.5, 1e3, .inf, 2026-11-15) is refused, so what the reviewer saw is what the schema sees: write
// it in quotes if it is text.
func (r *reader) scalar(n *yaml.Node) (any, error) {
	s := n.Value
	if !utf8.ValidString(s) {
		return nil, strict("text that is not UTF-8")
	}
	for _, c := range s {
		if (unicode.IsControl(c) && c != '\n' && c != '\t' && c != '\r') || c == 0xFEFF {
			return nil, strict("a control character (U+%04X) in a value", c)
		}
	}
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return s, nil
	}
	switch s {
	case "", "~", "null", "Null", "NULL":
		return nil, nil
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	if yaml11Bool[s] {
		return nil, strict("%s is a boolean in YAML 1.1 and a string in YAML 1.2: quote it if it is text, or write true or false", s)
	}
	if plainInt.MatchString(s) {
		digits := strings.TrimLeft(s, "+-")
		if len(digits) > MaxIntDigits {
			return nil, strict("the integer %s has more than %d digits", s, MaxIntDigits)
		}
		n, _ := strconv.ParseInt(s, 10, 64)
		if n > 1<<53 || n < -(1<<53) {
			return nil, strict("the integer %s is larger than a definition needs", s)
		}
		return n, nil
	}
	if simpleFloat.MatchString(s) {
		f, _ := strconv.ParseFloat(s, 64) // a fraction reads the same in YAML 1.1 and 1.2; no definition field takes one
		return f, nil
	}
	if yaml11Int.MatchString(s) || yaml11Flt.MatchString(s) || yaml12Num.MatchString(s) || timestamp.MatchString(s) {
		return nil, strict("%s is read as a number or a date by some YAML readers and as text by others: quote it if it is text; a definition has whole numbers only", s)
	}
	return s, nil
}
