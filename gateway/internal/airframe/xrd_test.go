package airframe

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The field tables of xr.request are held to the XRDs they come from: testdata/xrds/*.yaml are copies of Airframe's xrds/ at commit
// 4131191 (the commit the vendored contract was made from). Every field a table accepts must be in its XRD, with the same type, an
// enum that is a subset, a pattern that is the XRD's, and bounds at least as tight; no accepted object is free-form. A change to an XRD
// copy, or to a table, that makes them disagree fails here.

func loadXRD(t *testing.T, kind string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "xrds", strings.ToLower(kind)+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	vs := stAsList(stAsMap(doc["spec"])["versions"])
	if len(vs) != 1 {
		t.Fatalf("%s: %d versions", kind, len(vs))
	}
	sch := stAsMap(stAsMap(stAsMap(stAsMap(vs[0])["schema"])["openAPIV3Schema"])["properties"])
	return stAsMap(sch["spec"])
}

func asF(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func anchored(p string) string {
	return `\A(?:` + strings.TrimSuffix(strings.TrimPrefix(p, "^"), "$") + `)\z`
}

func compareRule(t *testing.T, path string, r *rule, x map[string]any) {
	t.Helper()
	if x == nil {
		t.Errorf("%s: not in the XRD", path)
		return
	}
	if x["x-kubernetes-preserve-unknown-fields"] == true {
		t.Errorf("%s: the XRD's field is free-form (x-kubernetes-preserve-unknown-fields) and is accepted", path)
	}
	typ, _ := x["type"].(string)
	switch r.t {
	case tString, tSelector:
		if typ != "string" {
			t.Errorf("%s: a string where the XRD has %q", path, typ)
		}
	case tInt:
		if typ != "integer" {
			t.Errorf("%s: an integer where the XRD has %q", path, typ)
		}
	case tNumber:
		if typ != "number" && typ != "integer" {
			t.Errorf("%s: a number where the XRD has %q", path, typ)
		}
	case tBool:
		if typ != "boolean" {
			t.Errorf("%s: a boolean where the XRD has %q", path, typ)
		}
	case tCIDR:
		if typ != "string" {
			t.Errorf("%s: a range where the XRD has %q", path, typ)
		}
	case tArray:
		if typ != "array" {
			t.Errorf("%s: a list where the XRD has %q", path, typ)
		}
		if m, ok := asF(x["maxItems"]); ok && float64(r.maxItems) > m {
			t.Errorf("%s: maxItems %d above the XRD's %v", path, r.maxItems, m)
		}
		if _, ok := asF(x["maxItems"]); !ok && r.t == tArray && r.maxItems == 0 {
			t.Errorf("%s: a list with no bound", path)
		}
		items := stAsMap(x["items"])
		if r.item != nil && r.item.t == tCIDR {
			if items["type"] != "string" {
				t.Errorf("%s: items are not strings", path)
			}
		} else if r.item != nil {
			compareRule(t, path+"[]", r.item, items)
		}
	case tObject:
		if typ != "object" {
			t.Errorf("%s: a mapping where the XRD has %q", path, typ)
		}
		if ap, has := x["additionalProperties"]; has && ap != false {
			t.Errorf("%s: the XRD's mapping takes any key (additionalProperties) and its fields are accepted", path)
		}
		props := stAsMap(x["properties"])
		for name, fr := range r.fields {
			compareRule(t, path+"."+name, fr, stAsMap(props[name]))
		}
		for _, req := range stAsList(x["required"]) {
			name, _ := req.(string)
			if r.fields[name] != nil && !stHas(r.required, name) && path != "spec" {
				t.Errorf("%s: the XRD requires %s and the table does not", path, name)
			}
		}
	}
	if r.t == tString || r.t == tCIDR {
		if enum := stAsList(x["enum"]); len(enum) > 0 && len(r.enumS) > 0 {
			for _, e := range r.enumS {
				found := false
				for _, xe := range enum {
					found = found || fmt.Sprint(xe) == e
				}
				if !found {
					t.Errorf("%s: enum value %q is not in the XRD's", path, e)
				}
			}
		} else if len(enum) > 0 {
			t.Errorf("%s: the XRD has an enum and the table does not", path)
		}
		if p, _ := x["pattern"].(string); p != "" {
			if r.re == nil {
				if r.t != tCIDR {
					t.Errorf("%s: the XRD has the pattern %s and the table has none", path, p)
				}
			} else if r.re.String() != anchored(p) {
				t.Errorf("%s: pattern %s is not the XRD's %s", path, r.re.String(), anchored(p))
			}
		}
		if m, ok := asF(x["maxLength"]); ok && r.t == tString && (r.maxLen == 0 || float64(r.maxLen) > m) {
			t.Errorf("%s: maxLength %d is above the XRD's %v", path, r.maxLen, m)
		}
	}
	if r.t == tInt || r.t == tNumber {
		if enum := stAsList(x["enum"]); len(enum) > 0 && len(r.enumI) > 0 {
			for _, e := range r.enumI {
				found := false
				for _, xe := range enum {
					found = found || fmt.Sprint(xe) == fmt.Sprint(e)
				}
				if !found {
					t.Errorf("%s: %d is not in the XRD's enum", path, e)
				}
			}
		}
		if x["exclusiveMinimum"] == true && !r.exclMin {
			t.Errorf("%s: the XRD's minimum is exclusive and the table's is not", path)
		}
		if mn, ok := asF(x["minimum"]); ok && r.ranged && r.min < mn {
			t.Errorf("%s: minimum %v is below the XRD's %v", path, r.min, mn)
		}
		if mx, ok := asF(x["maximum"]); ok && r.ranged && r.max > mx {
			t.Errorf("%s: maximum %v is above the XRD's %v", path, r.max, mx)
		}
		if _, ok := asF(x["maximum"]); !ok && len(r.enumI) == 0 && !r.ranged {
			t.Errorf("%s: a number with no bound", path)
		}
	}
}

func stHas(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestEveryFieldTheTablesAcceptIsInTheXRDAtLeastAsStrict(t *testing.T) {
	for kind, tbl := range kindFields {
		t.Run(kind, func(t *testing.T) {
			x := loadXRD(t, kind)
			compareRule(t, "spec", tbl, x)
			// every required top-level field is one the request has: the agent's own, filled in, or required by the table
			for _, req := range stAsList(x["required"]) {
				name, _ := req.(string)
				fixed := fixedFields(kind, "a", "dev", Registry{DevCluster: "c"})
				if _, isFixed := fixed.vals[name]; isFixed || name == "environmentRef" && fixed.envRef {
					continue
				}
				if !stHas(requiredFields[kind], name) {
					t.Errorf("the XRD requires spec.%s and the request does not", name)
				}
			}
			// the XRD's own platform machinery is not a field of any table
			for _, k := range []string{"compositionRef", "providerConfigRef", "writeConnectionSecretToRef", "managementPolicies", "deletionPolicy", "chart", "crossplane"} {
				if tbl.fields[k] != nil {
					t.Errorf("%s is in the table", k)
				}
			}
		})
	}
}

func TestEveryKindHasItsXRDCopyAndNoOtherIsRequestable(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "xrds", "*.yaml"))
	if len(files) != len(kindFields) {
		t.Fatalf("%d XRD copies for %d kinds", len(files), len(kindFields))
	}
}
