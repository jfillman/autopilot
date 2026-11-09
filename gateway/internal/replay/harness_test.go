package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoRoot is the autopilot checkout this test lives in.
func repoRoot(t testing.TB) string {
	t.Helper()
	wd, _ := os.Getwd()
	root, err := filepath.Abs(filepath.Join(wd, "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// clearanceSrc finds a clearance checkout that has the golden corpus: CLEARANCE_SRC, HANGAR_TECH/clearance, or a
// sibling of the repo.
func clearanceSrc(t testing.TB) string {
	t.Helper()
	root := repoRoot(t)
	cands := []string{os.Getenv("CLEARANCE_SRC")}
	if h := os.Getenv("HANGAR_TECH"); h != "" {
		cands = append(cands, filepath.Join(h, "clearance"))
	}
	cands = append(cands, filepath.Join(root, "..", "clearance"))
	for _, c := range cands {
		if c == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, "testdata", "golden", "corpus.jsonl")); err == nil {
			return c
		}
	}
	return ""
}

const gatewayPrincipal = "autopilot-system/autopilot-gateway"

// coreProc is a running test core: `clearance core --memory --insecure-dev-auth --max-clock-skew 0` (the bearer token
// is the principal, and the chain it wrote is served at GET /v1/dev/records), as tests/clearance_core/coreenv.py runs it.
type coreProc struct {
	URL string
	cmd *exec.Cmd
}

func startCore(t testing.TB, bin string, withBundle bool) *coreProc {
	t.Helper()
	root := repoRoot(t)
	envFile, err := os.ReadFile(filepath.Join(root, "policy", "hangar", "core-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	var coreEnv map[string]string
	if err := json.Unmarshal(envFile, &coreEnv); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLEARANCE_") {
			env = append(env, kv)
		}
	}
	for k, v := range coreEnv {
		if !strings.HasPrefix(k, "$") {
			env = append(env, k+"="+v)
		}
	}
	providers, _ := json.Marshal([]map[string]any{{"name": "autopilot-gateway", "version": "test", "principals": []string{gatewayPrincipal}, "definitions": true}})
	env = append(env, "CLEARANCE_PROVIDERS="+string(providers), "CLEARANCE_AGENTS_DIR="+filepath.Join(root, "agents"))
	if withBundle {
		src, _ := json.Marshal(map[string]any{"glidepath": map[string]any{"file": filepath.Join(root, "tests", "fixtures", "glidepath-contract.json")}})
		env = append(env, "CLEARANCE_BUNDLES="+filepath.Join(root, "policy", "hangar", "bundle.json"), "CLEARANCE_BUNDLE_SOURCES="+string(src))
	}
	cmd := exec.Command(bin, "core", "--memory", "--insecure-dev-auth", "--max-clock-skew", "0", "--listen", "127.0.0.1:0")
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &coreProc{cmd: cmd}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		sent := false
		var tail []string
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil && m["msg"] == "clearance core serving" && !sent {
				addr <- fmt.Sprint(m["addr"])
				sent = true
			}
			tail = append(tail, sc.Text())
			if len(tail) > 20 {
				tail = tail[1:]
			}
		}
		if !sent {
			addr <- "ERR " + strings.Join(tail, "\n")
		}
	}()
	select {
	case a := <-addr:
		if strings.HasPrefix(a, "ERR ") {
			t.Fatalf("the test core did not start:\n%s", a)
		}
		c.URL = "http://" + a
	case <-time.After(60 * time.Second):
		t.Fatal("the test core did not start in 60s")
	}
	return c
}

// records reads the core's chain from seq `from` (GET /v1/dev/records).
func (c *coreProc) records(t testing.TB, from int64) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/dev/records?from=%d&limit=10000", c.URL, from), nil)
	req.Header.Set("Authorization", "Bearer "+gatewayPrincipal)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("records: %v: %s", err, b)
	}
	return out
}

// head is the seq the next record gets.
func (c *coreProc) head(t testing.TB) int64 {
	recs := c.records(t, 0)
	if len(recs) == 0 {
		return 0
	}
	return int64(recs[len(recs)-1]["seq"].(float64)) + 1
}
