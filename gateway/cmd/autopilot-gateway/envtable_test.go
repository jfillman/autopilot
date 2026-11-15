package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/otlplogs"
)

// gateway/envtable.json is every environment variable the Python gateway and autopilot-api read, against the Go gateway's. These tests are
// what keeps it true, and fail the build when a variable the Python Deployment sets today would be silently ignored by Go.

type envVar struct {
	Name     string `json:"name"`
	Python   string `json:"python"`
	Go       string `json:"go"`
	Class    string `json:"class"`
	Deployed string `json:"deployed"`
	Example  string `json:"example"`
	Via      string `json:"via"`
	Note     string `json:"note"`
}

func loadEnvTable(t *testing.T) []envVar {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "envtable.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Vars []envVar `json:"vars"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, v := range doc.Vars {
		if seen[v.Name] {
			t.Fatalf("%s is in the table twice", v.Name)
		}
		seen[v.Name] = true
	}
	return doc.Vars
}

func has(csv, p string) bool {
	for _, x := range strings.Split(csv, ",") {
		if strings.TrimSpace(x) == p {
			return true
		}
	}
	return false
}

// recorder is an environment that remembers what was asked of it.
type recorder struct {
	m    map[string]string
	read map[string]bool
}

func (r *recorder) get(k string) string { r.read[k] = true; return r.m[k] }

// The environment the Python Deployments set today (the `deployed` rows, with the values apron renders), plus what the Go gateway needs
// beside it (the rows that have an example and are not deployed: the plain-http switches, the broker, the owners, the namespaces), must start
// a Go serve and a Go api, and every variable the Python Deployment sets must be READ by the Go process that has it: none is silently ignored.
func TestEveryVariableThePythonDeploymentSetsIsReadByGo(t *testing.T) {
	table := loadEnvTable(t)
	for _, proc := range []string{"serve", "api"} {
		env := map[string]string{}
		for _, v := range table {
			if v.Example == "" {
				continue
			}
			if v.Deployed != "" && !has(v.Deployed, proc) && v.Deployed != "both" {
				continue
			}
			if v.Deployed == "" && !has(v.Go, proc) {
				continue
			}
			env[v.Name] = v.Example
		}
		// the Go-only requirements of a serve, which a Python Deployment has no counterpart for
		rec := &recorder{m: env, read: map[string]bool{}}
		var err error
		if proc == "serve" {
			_, err = configFromEnv(rec.get)
		} else {
			_, err = apiConfigFromEnv(rec.get)
		}
		if err != nil {
			t.Fatalf("a Go %s refuses the Python Deployment's environment (plus the Go-only settings): %v", proc, err)
		}
		if _, err := logSettingsFromEnv(rec.get); err != nil {
			t.Fatalf("the log export: %v", err)
		}
		otlplogs.ResourceAttributes(rec.get, "autopilot-"+proc)
		checked := 0
		defer func(proc string) {
			if checked < 20 {
				t.Errorf("only %d deployed variables of the %s were checked: the test is not looking at the Deployment", checked, proc)
			}
		}(proc)
		for _, v := range table {
			deployed := v.Deployed == "both" || has(v.Deployed, proc)
			if !deployed || v.Class == "kubelet" {
				continue
			}
			checked++
			if v.Go == "" || !has(v.Go, proc) {
				t.Errorf("%s is set on the Python %s Deployment and the Go %s does not read it: it would be silently ignored (class %q)", v.Name, proc, proc, v.Class)
				continue
			}
			if v.Via == "os.Getenv" {
				continue
			}
			if !rec.read[v.Name] {
				t.Errorf("%s is set on the Python %s Deployment, the table says Go %s reads it, and the start did not ask for it", v.Name, proc, proc)
			}
		}
	}
}

// Every deployed row has a value to test with, a refusal class is refused, and a class says what it means.
func TestTheEnvTableIsWellFormed(t *testing.T) {
	for _, v := range loadEnvTable(t) {
		switch v.Class {
		case "same", "new", "not_carried", "refused", "retired", "kubelet":
		default:
			t.Errorf("%s: class %q", v.Name, v.Class)
		}
		if v.Deployed != "" && v.Example == "" {
			t.Errorf("%s is deployed and has no example value", v.Name)
		}
		if v.Class == "not_carried" && (v.Note == "" || v.Deployed != "") {
			t.Errorf("%s: a variable Go does not carry needs a reason, and cannot be one the Deployment sets", v.Name)
		}
		if v.Class == "new" && v.Python != "" {
			t.Errorf("%s is new in Go and the table says Python reads it", v.Name)
		}
		if v.Class == "refused" || v.Class == "retired" {
			if v.Go == "" {
				t.Errorf("%s is refused and no Go process is said to refuse it", v.Name)
			}
		}
	}
}

// A refused or retired variable is refused by the Go processes the table names, by name.
func TestTheRefusedVariablesAreRefusedByGo(t *testing.T) {
	for _, v := range loadEnvTable(t) {
		if v.Class != "refused" && v.Class != "retired" {
			continue
		}
		for _, proc := range []string{"serve", "api"} {
			if !has(v.Go, proc) {
				continue
			}
			var err error
			if proc == "serve" {
				_, err = configFromEnv(envOf(base(map[string]string{v.Name: "1"})))
			} else {
				_, err = apiConfigFromEnv(envOf(map[string]string{"GATEWAY_API_DSN": "postgres://x", v.Name: "1"}))
			}
			if err == nil || !strings.Contains(err.Error(), v.Name) {
				t.Errorf("Go %s does not refuse %s by name: %v", proc, v.Name, err)
			}
		}
	}
}

var envLiteral = regexp.MustCompile(`"([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)"`)

// Literals that look like a variable and are not one.
var notEnv = map[string]bool{"CLEARANCE_MODEL": true}

// The other direction: every variable the Go source names is in the table (a new setting cannot be added without saying what it is to the
// Python gateway), and every variable the table says Go reads is in the source.
func TestTheEnvTableNamesEveryVariableTheGoSourceReads(t *testing.T) {
	table := loadEnvTable(t)
	inTable := map[string]envVar{}
	for _, v := range table {
		inTable[v.Name] = v
	}
	found := map[string]bool{}
	for _, root := range []string{".", filepath.Join("..", "..", "internal")} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			if root == "." && strings.Contains(p, string(filepath.Separator)) {
				return nil
			}
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), `"PORT"`) {
				found["PORT"] = true // the one name without an underscore
			}
			for _, m := range envLiteral.FindAllStringSubmatch(string(b), -1) {
				if !notEnv[m[1]] {
					found[m[1]] = true
				}
			}
			return nil
		})
	}
	var missing []string
	for n := range found {
		if _, ok := inTable[n]; !ok {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the Go source names variables that gateway/envtable.json does not: %v (say what each is to the Python gateway)", missing)
	}
	for _, v := range table {
		if v.Go != "" && !found[v.Name] {
			t.Errorf("the table says Go reads %s and the source never names it", v.Name)
		}
	}
	if len(found) < 100 {
		t.Fatalf("only %d variables were found in the source: the scan is not looking at it", len(found))
	}
}
