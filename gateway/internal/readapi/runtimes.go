package readapi

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"go.yaml.in/yaml/v3"
)

// Runtime is one entry of the runtime catalog (engine/runtimes.py).
type Runtime struct {
	Name, Kind, Framework, Summary, Status string
	Image                                  *string
}

// View is Runtime.view().
func (r Runtime) View() Record {
	var img any
	if r.Image != nil {
		img = *r.Image
	}
	return Record{"name": r.Name, "image": img, "kind": r.Kind, "framework": r.Framework, "summary": r.Summary, "status": r.Status}
}

var (
	digestRe  = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$`)
	runtimeRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// LoadRuntimes reads runtime/catalog.yaml: none when the file is not there (an image without one lists none), a bad one is an error
// (a malformed catalog stops the start, as Python's ConfigError does).
func LoadRuntimes(path string) ([]Runtime, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runtime catalog %s: %w", path, err)
	}
	var doc struct {
		Runtimes []map[string]any `yaml:"runtimes"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("runtime catalog %s: %w", path, err)
	}
	var out []Runtime
	seen := map[string]bool{}
	for _, e := range doc.Runtimes {
		str := func(k, d string) string {
			if v, ok := e[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
			return d
		}
		name, _ := e["name"].(string)
		status, kind := str("status", "available"), str("kind", "runtime")
		if !runtimeRe.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("runtime catalog %s: runtime catalog: bad or duplicate name %q", path, name)
		}
		if (status != "available" && status != "planned") || (kind != "runtime" && kind != "base") {
			return nil, fmt.Errorf("runtime catalog %s: runtime catalog: %s: status is available or planned, kind is runtime or base", path, name)
		}
		image, hasImage := e["image"].(string)
		if status == "available" && (!hasImage || !digestRe.MatchString(image)) {
			return nil, fmt.Errorf("runtime catalog %s: runtime catalog: %s: an available runtime is pinned by digest", path, name)
		}
		seen[name] = true
		r := Runtime{Name: name, Kind: kind, Framework: str("framework", "generic"), Summary: str("summary", ""), Status: status}
		if r.Framework == "" {
			r.Framework = "generic"
		}
		if hasImage {
			r.Image = &image
		}
		out = append(out, r)
	}
	return out, nil
}
