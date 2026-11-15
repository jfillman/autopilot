package airframe

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
	"go.yaml.in/yaml/v3"
)

// DumpYAML is yaml.safe_dump(v, sort_keys=False) for the ordered value model: block style, keys in their order, two-space indent,
// every string that would read as another type (yes, 1, null, a date) quoted, a multi-line string as a literal block, no folding
// of a long line. It is the text of a file the gateway writes, so it must read back, with the strict reader, as the same value
// (TestDumpYAMLRoundTrips and FuzzDumpYAML).
func DumpYAML(v any) string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(yamlNode(v)); err != nil {
		panic("DumpYAML: " + err.Error())
	}
	_ = enc.Close()
	return buf.String()
}

func yamlNode(v any) *yaml.Node {
	switch x := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(x)}
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(x, 10)}
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: yamlFloat(x)}
	case string:
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x}
		if !strings.Contains(x, "\n") && !plainOK(x) {
			n.Style = yaml.DoubleQuotedStyle
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range x {
			n.Content = append(n.Content, yamlNode(e))
		}
		return n
	case *definitions.OMap:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range x.Keys {
			n.Content = append(n.Content, yamlNode(k), yamlNode(x.M[k]))
		}
		return n
	}
	panic(fmt.Sprintf("DumpYAML: %T is not a value of the ordered model", v))
}

// yamlFloat is a float the strict reader reads back as a float: with a point (YAML 1.1 has no 1e+20, PyYAML writes 1.0e+20).
func yamlFloat(f float64) string {
	s := pyrepr.Float(f)
	if !strings.Contains(s, ".") {
		if i := strings.IndexAny(s, "eE"); i >= 0 {
			return s[:i] + ".0" + s[i:]
		}
		return s + ".0"
	}
	return s
}

// plainSafe is text no YAML reader (1.1 or 1.2) takes for anything but a string when it is written without quotes: it starts with a
// letter, holds letters, digits and a few separators, and is not one of the words YAML 1.1 reads as a boolean or null. Every other
// string is written in double quotes, which every reader reads as text.
var plainSafe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _./,()-]*$`)

var reservedWords = map[string]bool{"y": true, "yes": true, "n": true, "no": true, "on": true, "off": true, "true": true, "false": true, "null": true}

func plainOK(s string) bool {
	return plainSafe.MatchString(s) && !reservedWords[strings.ToLower(s)] && s == strings.TrimSpace(s) && !strings.Contains(s, "  ")
}
