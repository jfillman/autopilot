package definitions

import (
	"errors"
	"fmt"
	"sort"
)

// A Schema is any JSON-schema document this validator can read (the AppSpec schema, A-9 S5c), with the same reading of jsonschema's
// errors and best_match the definition schema has. A document that uses a keyword the validator does not implement is refused
// when it is read, never silently half-checked.
type Schema struct{ root *OMap }

// Err is one validation error as jsonschema reports it: where (the absolute path of keys and indexes; indexes are int64) and what.
type Err struct {
	Path    []any
	Message string
}

// NewSchema reads a schema document.
func NewSchema(raw []byte) (s *Schema, err error) {
	defer func() {
		if r := recover(); r != nil {
			s, err = nil, fmt.Errorf("the schema is not one this validator can read: %v", r)
		}
	}()
	v, derr := DecodeJSON(raw)
	if derr != nil {
		return nil, derr
	}
	root, ok := v.(*OMap)
	if !ok {
		return nil, errors.New("the schema is not an object")
	}
	checkKeywords(root)
	return &Schema{root: root}, nil
}

func toErr(e *verr) *Err { return &Err{Path: e.abs(), Message: e.msg} }

// Best is jsonschema.validate's error: best_match over every error, or nil when the instance is valid.
func (s *Schema) Best(inst any) *Err {
	b := bestMatch((&vctx{root: s.root}).errorsOf(inst, s.root))
	if b == nil {
		return nil
	}
	return toErr(b)
}

// ErrorsAt is iter_errors of a validator whose schema is {"$defs": ..., "$ref": ref}: every error against the definition ref names
// (`#/$defs/chartValues`), in jsonschema's order.
func (s *Schema) ErrorsAt(ref string, inst any) []*Err {
	c := &vctx{root: s.root}
	target := c.resolve(ref)
	if target == nil {
		panic("the schema has no " + ref)
	}
	var out []*Err
	for _, e := range c.errorsOf(inst, target) {
		out = append(out, toErr(e))
	}
	return out
}

// SortByPath orders errors the way Python's `sorted(errors, key=lambda x: list(x.absolute_path))` does for paths that compare
// (a key against a key, an index against an index); a key sorts before an index where Python would raise.
func SortByPath(errs []*Err) {
	sort.SliceStable(errs, func(i, j int) bool { return lessPath(errs[i].Path, errs[j].Path) })
}

func lessPath(a, b []any) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		switch x := a[i].(type) {
		case string:
			y, ok := b[i].(string)
			if !ok {
				return true
			}
			if x != y {
				return x < y
			}
		case int64:
			y, ok := b[i].(int64)
			if !ok {
				return false
			}
			if x != y {
				return x < y
			}
		}
	}
	return len(a) < len(b)
}
