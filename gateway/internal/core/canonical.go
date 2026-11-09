package core

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strconv"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// appendCanonical writes v as Python's json.dumps(sort_keys=True, separators=(",", ":"), ensure_ascii=False).
func appendCanonical(out []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(out, "null"...)
	case bool:
		return strconv.AppendBool(out, x)
	case json.Number:
		return append(out, x.String()...)
	case string:
		return appendString(out, x)
	case []any:
		out = append(out, '[')
		for i, e := range x {
			if i > 0 {
				out = append(out, ',')
			}
			out = appendCanonical(out, e)
		}
		return append(out, ']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, '{')
		for i, k := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			out = appendString(out, k)
			out = append(out, ':')
			out = appendCanonical(out, x[k])
		}
		return append(out, '}')
	}
	return out
}

// appendString escapes as Python does with ensure_ascii=False: ", \ and control characters only.
func appendString(out []byte, s string) []byte {
	out = append(out, '"')
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, `\"`...)
		case '\\':
			out = append(out, `\\`...)
		case '\n':
			out = append(out, `\n`...)
		case '\r':
			out = append(out, `\r`...)
		case '\t':
			out = append(out, `\t`...)
		case '\b':
			out = append(out, `\b`...)
		case '\f':
			out = append(out, `\f`...)
		default:
			if r < 0x20 {
				out = append(out, []byte(`\u00`+hex2(byte(r)))...)
			} else {
				out = append(out, string(r)...)
			}
		}
	}
	return append(out, '"')
}

func hex2(b byte) string {
	const h = "0123456789abcdef"
	return string([]byte{h[b>>4], h[b&15]})
}
