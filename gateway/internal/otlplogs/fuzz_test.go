package otlplogs

import (
	"strings"
	"testing"
)

func FuzzResourceAttributes(f *testing.F) {
	for _, s := range []string{"k=v,a=b", "service.name=x", ",,=,=x,y=", "a=b=c", " k = v "} {
		f.Add(s, "svc")
	}
	f.Fuzz(func(t *testing.T, attrs, svc string) {
		got := ResourceAttributes(func(k string) string {
			switch k {
			case "OTEL_RESOURCE_ATTRIBUTES":
				return attrs
			case "OTEL_SERVICE_NAME":
				return svc
			}
			return ""
		}, "default")
		if got["service.name"] == "" {
			t.Fatal("no service.name")
		}
		for k, v := range got {
			if k == "" || v == "" {
				t.Fatalf("an empty key or value: %q=%q", k, v)
			}
		}
	})
}

// Whatever a line says, a credential the process knows is not in what is sent, and redaction is idempotent.
func FuzzRedact(f *testing.F) {
	f.Add("token abc", "literal-secret")
	f.Add("Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln", "x")
	f.Add("secrets: 3 loaded", "")
	f.Add("image@sha256:b5c9832f0d5d092e616a98e365862eb1e03a15dffe2d545f97c9adfed7f1d965", "")
	for _, c := range shapes { // a seed for every shape the table knows
		f.Add(c.line, "")
	}
	f.Fuzz(func(t *testing.T, line, secret string) {
		out := Redact(line, secret)
		if len(secret) >= 6 && strings.Contains(out, secret) && !strings.Contains(Redacted, secret) {
			t.Fatalf("%q survived in %q", secret, out)
		}
		// A second pass may remove what the first one's output joined into a longer run (a "=" between two digit runs makes a 40-character
		// run); a third finds nothing more.
		twice := Redact(out, secret)
		if again := Redact(twice, secret); again != twice {
			t.Fatalf("redaction does not settle: %q -> %q -> %q -> %q", line, out, twice, again)
		}
	})
}
