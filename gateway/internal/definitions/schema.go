package definitions

import (
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
)

// The definition schema (src/autopilot/engine/schemas/agent-definition.schema.json, copied here by tools/gen_gateway_definitions.py,
// which fails its --check when the copy differs) is the JSON-schema document the Python loader validates every definition against
// (engine/profile.py parse -> jsonschema.validate). This is a validator for the keywords that document uses, read from that file,
// so a change to the schema is a change here without anyone remembering to make it: a keyword this validator does not know makes
// it panic at start, and a test holds the copy equal to Python's.
//
// It reports what Python reports, in Python's words. jsonschema.validate raises best_match(iter_errors(...)): errors come in the
// order the schema lists its keywords, the shallowest one wins (then the one whose path sorts last, then a non-oneOf one, then one
// whose instance has the type the schema asked for), and an error from oneOf is replaced by the best of its branches' errors when
// they are not all equally good.
//
// Two places it is deliberately not Python (the fixtures say which cases): a pattern's `$` matches only at the very end of the text
// (Python's also matches before a final newline, so "gate-api\n" passes a Python `^...$`), and an integer is a whole number written
// as one (Python also takes 20.0).

//go:embed agent-definition.schema.json
var schemaJSON []byte

var theSchema = mustSchema()

func mustSchema() *OMap {
	v, err := DecodeJSON(schemaJSON)
	if err != nil {
		panic("the definition schema does not parse: " + err.Error())
	}
	root, ok := v.(*OMap)
	if !ok {
		panic("the definition schema is not an object")
	}
	checkKeywords(root)
	return root
}

var known = map[string]bool{
	"$schema": true, "title": true, "description": true, "type": true, "additionalProperties": true, "required": true, "properties": true,
	"pattern": true, "minLength": true, "maxLength": true, "enum": true, "items": true, "uniqueItems": true, "minimum": true, "maximum": true,
	"maxItems": true, "minProperties": true, "const": true, "oneOf": true, "if": true, "then": true, "propertyNames": true,
	// A-9 S5c (the AppSpec schema): references to local definitions, the other combinators, and the bounds it uses. `format` is
	// never checked (jsonschema has no format checker unless one is passed). `$defs`, `default`, `$comment` and every `x-` key are
	// annotations.
	"$ref": true, "anyOf": true, "allOf": true, "not": true, "exclusiveMinimum": true, "minItems": true, "maxProperties": true,
	"format": true, "$defs": true, "default": true, "$comment": true,
}

func isKnownKeyword(k string) bool { return known[k] || strings.HasPrefix(k, "x-") }

// checkKeywords panics for a keyword in a schema position that this validator would silently ignore.
func checkKeywords(s *OMap) {
	for _, k := range s.Keys {
		if !isKnownKeyword(k) {
			panic("the definition schema uses the keyword " + k + ", which the Go validator does not implement")
		}
		v := s.M[k]
		switch k {
		case "$defs":
			for _, dk := range v.(*OMap).Keys {
				checkKeywords(v.(*OMap).M[dk].(*OMap))
			}
		case "not":
			checkKeywords(v.(*OMap))
		case "$ref":
			if _, ok := v.(string); !ok || !strings.HasPrefix(v.(string), "#/") {
				panic("the schema has a $ref that is not local: " + pyrepr.Value(v))
			}
		case "anyOf", "allOf":
			for _, e := range v.([]any) {
				checkKeywords(e.(*OMap))
			}
		case "properties":
			for _, pk := range v.(*OMap).Keys {
				checkKeywords(v.(*OMap).M[pk].(*OMap))
			}
		case "items", "if", "then", "propertyNames":
			checkKeywords(v.(*OMap))
		case "additionalProperties":
			if o, ok := v.(*OMap); ok {
				checkKeywords(o)
			}
		case "oneOf":
			for _, e := range v.([]any) {
				checkKeywords(e.(*OMap))
			}
		case "pattern":
			compile(v.(string))
		}
	}
}

// pyWhite is what Python's `\s` matches in a str pattern: its unicode white space (Go's is ASCII only, and a name that holds a
// no-break space must not pass `[^\s@]`).
const pyWhite = `\t-\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var patterns sync.Map // pattern -> *regexp.Regexp

// CompilePattern is a schema `pattern` as Python's re would read it (translatePattern); it panics on one it cannot map.
func CompilePattern(p string) *regexp.Regexp { return compile(p) }

func compile(p string) *regexp.Regexp {
	if re, ok := patterns.Load(p); ok {
		return re.(*regexp.Regexp)
	}
	g := translatePattern(p)
	re, err := regexp.Compile(g)
	if err != nil {
		panic("a schema pattern does not compile: " + p + ": " + err.Error())
	}
	patterns.Store(p, re)
	return re
}

// translatePattern rewrites the one escape whose meaning differs between Python's `re` (str pattern: Unicode) and Go's RE2
// (ASCII): `\s` is pyWhite wherever it stands. Outside a character class `\s` becomes `[pyWhite]` and `\S` becomes `[^pyWhite]`;
// inside a class `\s` becomes the bare set. `\S` inside a class has no faithful RE2 form (a negated set inside a set), so it
// panics, as does any other construct this does not map. An escaped backslash (`\\`) is skipped as a pair, so `\\s` is a
// backslash then an `s`, left alone. Every other escape passes through untouched.
func translatePattern(p string) string {
	var out strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			n := p[i+1]
			i++
			switch {
			case n == 's' && inClass:
				out.WriteString(pyWhite)
			case n == 's':
				out.WriteString("[" + pyWhite + "]")
			case n == 'S' && inClass:
				panic("a schema pattern uses \\S inside a character class: " + p)
			case n == 'S':
				out.WriteString("[^" + pyWhite + "]")
			default:
				out.WriteByte('\\')
				out.WriteByte(n)
			}
		case c == '[' && !inClass:
			inClass = true
			out.WriteByte(c)
			// a leading ^ and a leading ] are part of the class opener, not its end
			if i+1 < len(p) && p[i+1] == '^' {
				out.WriteByte('^')
				i++
			}
			if i+1 < len(p) && p[i+1] == ']' {
				out.WriteString("\\]")
				i++
			}
		case c == ']' && inClass:
			inClass = false
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// verr is one validation error as jsonschema makes it.
type verr struct {
	msg       string
	validator string
	path      []any // relative to the instance its parent (or the document) validated
	context   []*verr
	instance  any
	schema    *OMap
	parent    *verr
}

func (e *verr) abs() []any {
	if e.parent == nil {
		return e.path
	}
	return append(append([]any{}, e.parent.abs()...), e.path...)
}

func isType(v any, t string) bool {
	switch t {
	case "object":
		_, ok := v.(*OMap)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		_, ok := v.(int64)
		return ok
	case "number":
		switch v.(type) {
		case int64, float64:
			return true
		}
		return false
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	}
	panic("the definition schema names the type " + t)
}

func equal(a, b any) bool {
	switch x := a.(type) {
	case *OMap:
		y, ok := b.(*OMap)
		if !ok || len(x.Keys) != len(y.Keys) {
			return false
		}
		for _, k := range x.Keys {
			yv, ok := y.M[k]
			if !ok || !equal(x.M[k], yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case int64:
		switch y := b.(type) {
		case int64:
			return x == y
		case float64:
			return float64(x) == y
		}
		return false
	case float64:
		switch y := b.(type) {
		case int64:
			return x == float64(y)
		case float64:
			return x == y
		}
		return false
	}
	return a == b
}

func num(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// vctx is one validation run: the schema document `$ref`s are resolved against.
type vctx struct{ root *OMap }

// resolve is the schema a local `$ref` (`#/a/b`, JSON pointer) names.
func (c *vctx) resolve(ref string) *OMap {
	var cur any = c.root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		o, ok := cur.(*OMap)
		if !ok {
			return nil
		}
		cur = o.M[part]
	}
	o, _ := cur.(*OMap)
	return o
}

// errorsOf is iter_errors(instance) against s: every error, in jsonschema's order.
func (c *vctx) errorsOf(inst any, s *OMap) []*verr {
	var out []*verr
	add := func(msg, kw string) {
		out = append(out, &verr{msg: msg, validator: kw, instance: inst, schema: s})
	}
	for _, kw := range s.Keys {
		v := s.M[kw]
		switch kw {
		case "type":
			ts := []string{}
			switch t := v.(type) {
			case string:
				ts = append(ts, t)
			case []any:
				for _, e := range t {
					ts = append(ts, e.(string))
				}
			}
			ok := false
			for _, t := range ts {
				ok = ok || isType(inst, t)
			}
			if !ok {
				reprs := make([]string, len(ts))
				for i, t := range ts {
					reprs[i] = pyrepr.Str(t)
				}
				add(fmt.Sprintf("%s is not of type %s", pyrepr.Value(inst), strings.Join(reprs, ", ")), kw)
			}
		case "enum":
			found := false
			for _, e := range v.([]any) {
				found = found || equal(e, inst)
			}
			if !found {
				add(fmt.Sprintf("%s is not one of %s", pyrepr.Value(inst), pyrepr.Value(v)), kw)
			}
		case "const":
			if !equal(inst, v) {
				add(fmt.Sprintf("%s was expected", pyrepr.Value(v)), kw)
			}
		case "pattern":
			if str, ok := inst.(string); ok && !compile(v.(string)).MatchString(str) {
				add(fmt.Sprintf("%s does not match %s", pyrepr.Str(str), pyrepr.Str(v.(string))), kw)
			}
		case "minLength":
			if str, ok := inst.(string); ok && int64(utf8.RuneCountInString(str)) < v.(int64) {
				m := "is too short"
				if v.(int64) == 1 {
					m = "should be non-empty"
				}
				add(pyrepr.Str(str)+" "+m, kw)
			}
		case "maxLength":
			if str, ok := inst.(string); ok && int64(utf8.RuneCountInString(str)) > v.(int64) {
				m := "is too long"
				if v.(int64) == 0 {
					m = "is expected to be empty"
				}
				add(pyrepr.Str(str)+" "+m, kw)
			}
		case "minimum":
			if isType(inst, "number") && num(inst) < num(v) {
				add(fmt.Sprintf("%s is less than the minimum of %s", pyrepr.Value(inst), pyrepr.Value(v)), kw)
			}
		case "maximum":
			if isType(inst, "number") && num(inst) > num(v) {
				add(fmt.Sprintf("%s is greater than the maximum of %s", pyrepr.Value(inst), pyrepr.Value(v)), kw)
			}
		case "maxItems":
			if a, ok := inst.([]any); ok && int64(len(a)) > v.(int64) {
				m := "is too long"
				if v.(int64) == 0 {
					m = "is expected to be empty"
				}
				add(pyrepr.Value(inst)+" "+m, kw)
			}
		case "uniqueItems":
			if a, ok := inst.([]any); ok && v == true && !unique(a) {
				add(pyrepr.Value(inst)+" has non-unique elements", kw)
			}
		case "minProperties":
			if o, ok := inst.(*OMap); ok && int64(len(o.Keys)) < v.(int64) {
				m := "does not have enough properties"
				if v.(int64) == 1 {
					m = "should be non-empty"
				}
				add(pyrepr.Value(inst)+" "+m, kw)
			}
		case "required":
			if o, ok := inst.(*OMap); ok {
				for _, p := range v.([]any) {
					if _, has := o.M[p.(string)]; !has {
						add(pyrepr.Str(p.(string))+" is a required property", kw)
					}
				}
			}
		case "properties":
			if o, ok := inst.(*OMap); ok {
				ps := v.(*OMap)
				for _, p := range ps.Keys {
					if iv, has := o.M[p]; has {
						out = append(out, c.descend(iv, ps.M[p].(*OMap), p)...)
					}
				}
			}
		case "additionalProperties":
			o, ok := inst.(*OMap)
			if !ok {
				break
			}
			props, _ := s.M["properties"].(*OMap)
			var extras []string
			for _, k := range o.Keys {
				if props == nil || props.M[k] == nil {
					extras = append(extras, k)
				}
			}
			if sub, isObj := v.(*OMap); isObj {
				for _, k := range extras {
					out = append(out, c.descend(o.M[k], sub, k)...)
				}
			} else if v == false && len(extras) > 0 {
				sort.Strings(extras)
				reprs := make([]string, len(extras))
				for i, e := range extras {
					reprs[i] = pyrepr.Str(e)
				}
				verb := "was"
				if len(extras) > 1 {
					verb = "were"
				}
				add(fmt.Sprintf("Additional properties are not allowed (%s %s unexpected)", strings.Join(reprs, ", "), verb), kw)
			}
		case "propertyNames":
			if o, ok := inst.(*OMap); ok {
				for _, k := range o.Keys {
					out = append(out, c.errorsOf(k, v.(*OMap))...)
				}
			}
		case "items":
			if a, ok := inst.([]any); ok {
				for i, e := range a {
					out = append(out, c.descend(e, v.(*OMap), int64(i))...)
				}
			}
		case "$ref":
			target := c.resolve(v.(string))
			if target == nil {
				panic("the schema has a $ref that does not resolve: " + v.(string))
			}
			out = append(out, c.errorsOf(inst, target)...)
		case "allOf":
			for _, sub := range v.([]any) {
				out = append(out, c.errorsOf(inst, sub.(*OMap))...)
			}
		case "anyOf":
			var all []*verr
			valid := false
			for _, sub := range v.([]any) {
				errs := c.errorsOf(inst, sub.(*OMap))
				if len(errs) == 0 {
					valid = true
					break
				}
				all = append(all, errs...)
			}
			if !valid {
				e := &verr{msg: pyrepr.Value(inst) + " is not valid under any of the given schemas", validator: kw, instance: inst, schema: s, context: all}
				for _, c := range all {
					c.parent = e
				}
				out = append(out, e)
			}
		case "not":
			if len(c.errorsOf(inst, v.(*OMap))) == 0 {
				add(fmt.Sprintf("%s should not be valid under %s", pyrepr.Value(inst), pyrepr.Value(v)), kw)
			}
		case "exclusiveMinimum":
			if isType(inst, "number") && num(inst) <= num(v) {
				add(fmt.Sprintf("%s is less than or equal to the minimum of %s", pyrepr.Value(inst), pyrepr.Value(v)), kw)
			}
		case "minItems":
			if a, ok := inst.([]any); ok && int64(len(a)) < v.(int64) {
				m := "is too short"
				if v.(int64) == 1 {
					m = "should be non-empty"
				}
				add(pyrepr.Value(inst)+" "+m, kw)
			}
		case "maxProperties":
			if o, ok := inst.(*OMap); ok && int64(len(o.Keys)) > v.(int64) {
				m := "has too many properties"
				if v.(int64) == 0 {
					m = "is expected to be empty"
				}
				add(pyrepr.Value(inst)+" "+m, kw)
			}
		case "oneOf":
			var all []*verr
			first := -1
			subs := v.([]any)
			for i, sub := range subs {
				errs := c.errorsOf(inst, sub.(*OMap))
				if len(errs) == 0 {
					first = i
					break
				}
				all = append(all, errs...)
			}
			if first < 0 {
				e := &verr{msg: pyrepr.Value(inst) + " is not valid under any of the given schemas", validator: kw, instance: inst, schema: s, context: all}
				for _, c := range all {
					c.parent = e
				}
				out = append(out, e)
			} else {
				var more []string
				for _, sub := range subs[first+1:] {
					if len(c.errorsOf(inst, sub.(*OMap))) == 0 {
						more = append(more, pyrepr.Value(sub))
					}
				}
				if len(more) > 0 {
					more = append(more, pyrepr.Value(subs[first]))
					add(pyrepr.Value(inst)+" is valid under each of "+strings.Join(more, ", "), kw)
				}
			}
			// Valid under more than one branch is an error in jsonschema ("is valid under each of ..."); no two branches of this
			// schema's triggers can both accept one object (each requires a different `type` or `source`), and the schema is
			// held equal to Python's, so a change that made it possible is seen by the fixtures.
		case "if":
			if len(c.errorsOf(inst, v.(*OMap))) == 0 {
				if then, ok := s.M["then"].(*OMap); ok {
					out = append(out, c.errorsOf(inst, then)...)
				}
			}
		}
	}
	return out
}

func unique(a []any) bool {
	for i := range a {
		for j := 0; j < i; j++ {
			if equal(a[i], a[j]) {
				return false
			}
		}
	}
	return true
}

// descend validates a child and puts the child's key or index on the front of each error's path.
func (c *vctx) descend(inst any, s *OMap, elem any) []*verr {
	errs := c.errorsOf(inst, s)
	for _, e := range errs {
		e.path = append([]any{elem}, e.path...)
	}
	return errs
}

// matchesType is error._matches_type(): the instance has the type the failing schema asked for.
func (e *verr) matchesType() bool {
	t, ok := e.schema.M["type"]
	if !ok {
		return false
	}
	switch x := t.(type) {
	case string:
		return isType(e.instance, x)
	case []any:
		for _, s := range x {
			if isType(e.instance, s.(string)) {
				return true
			}
		}
	}
	return false
}

// cmpKey orders errors by jsonschema's relevance: -len(path), then the path, then "not weak" (oneOf is weak), then "not
// matches type". Negative means a sorts before b.
func cmpKey(a, b *verr) int {
	if len(a.path) != len(b.path) {
		if len(a.path) > len(b.path) { // -len(a) < -len(b)
			return -1
		}
		return 1
	}
	for i := range a.path {
		switch x := a.path[i].(type) {
		case string:
			y, _ := b.path[i].(string)
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		case int64:
			y, _ := b.path[i].(int64)
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	bi := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	if c := bi(a.validator != "oneOf" && a.validator != "anyOf") - bi(b.validator != "oneOf" && b.validator != "anyOf"); c != 0 {
		return c
	}
	return bi(!a.matchesType()) - bi(!b.matchesType())
}

// bestMatch is jsonschema.exceptions.best_match: the largest by relevance (the first of equals), then, while it has context, the
// smaller of its two smallest context errors, unless those two rank the same, when it stays.
func bestMatch(errs []*verr) *verr {
	var best *verr
	for _, e := range errs {
		if best == nil || cmpKey(e, best) > 0 {
			best = e
		}
	}
	for best != nil && len(best.context) > 0 {
		ctx := append([]*verr{}, best.context...)
		sort.SliceStable(ctx, func(i, j int) bool { return cmpKey(ctx[i], ctx[j]) < 0 })
		if len(ctx) >= 2 && cmpKey(ctx[0], ctx[1]) == 0 {
			return best
		}
		best = ctx[0]
	}
	return best
}

// ValidateSchema is jsonschema.validate(doc, schema): nil for a document that passes, else the error Python would raise, as the
// DefinitionError text profile.parse makes of it ("<name>: <path>: <message>").
func ValidateSchema(doc *OMap) error {
	best := bestMatch((&vctx{root: theSchema}).errorsOf(doc, theSchema))
	if best == nil {
		return nil
	}
	parts := make([]string, 0, 4)
	for _, p := range best.abs() {
		switch x := p.(type) {
		case string:
			parts = append(parts, x)
		case int64:
			parts = append(parts, fmt.Sprint(x))
		}
	}
	path := strings.Join(parts, "/")
	if path == "" {
		path = "<root>"
	}
	name := any("?")
	if v, ok := doc.M["name"]; ok {
		name = v
	}
	// Path is the same path with every name the schema does not itself list ("match" keys, say) taken out: it says where, never what.
	safe := make([]string, 0, len(parts))
	for i, p := range best.abs() {
		switch x := p.(type) {
		case string:
			if schemaNames[x] {
				safe = append(safe, x)
			} else {
				safe = append(safe, "*")
			}
		case int64:
			safe = append(safe, parts[i])
		}
	}
	sp := strings.Join(safe, "/")
	if sp == "" {
		sp = "<root>"
	}
	return &DefinitionError{Msg: fmt.Sprintf("%s: %s: %s", pyrepr.Text(name), path, best.msg), Path: sp}
}

// schemaNames are the property names the schema lists: the only names a published reason may carry.
var schemaNames = func() map[string]bool {
	m := map[string]bool{}
	var walk func(s *OMap)
	walk = func(s *OMap) {
		if ps, ok := s.M["properties"].(*OMap); ok {
			for _, k := range ps.Keys {
				m[k] = true
				walk(ps.M[k].(*OMap))
			}
		}
		for _, k := range []string{"items", "then", "if", "propertyNames", "additionalProperties"} {
			if o, ok := s.M[k].(*OMap); ok {
				walk(o)
			}
		}
		if l, ok := s.M["oneOf"].([]any); ok {
			for _, e := range l {
				walk(e.(*OMap))
			}
		}
	}
	walk(theSchema)
	return m
}()

// DefinitionError is a document that does not parse as a definition (profile.DefinitionError).
type DefinitionError struct {
	Msg  string
	Path string // where, in schema terms, with no name that came from the document
}

func (e *DefinitionError) Error() string { return e.Msg }
