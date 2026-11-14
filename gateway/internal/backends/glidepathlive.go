package backends

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// glidepath.live.list, glidepath.live.logs.get and glidepath.live.watch (glidepath_live.py and the live parts of
// glidepath_api.py): what an agent reads of runs in flight.
//
// glidepath-api's live reads return PipelineRuns and TaskRuns as the cluster holds them, whole Kubernetes objects of tens of
// kilobytes each. An agent needs much less: which runs there are, where each is, which task failed, and the step containers
// whose live logs it may read. summarize is that view. A watch is the stream as one bounded tool call: it reads the stream's
// snapshot, then its change events, until the named PipelineRun finishes or the deadline passes, and answers the run as it is
// then and every task change it saw; when the stream cannot be opened it polls `live` every few seconds until the same
// deadline, so the agent gets the same answer either way.

const (
	labelPipeline     = "tekton.dev/pipeline"
	labelPipelineRun  = "tekton.dev/pipelineRun"
	labelPipelineTask = "tekton.dev/pipelineTask"
	maxChanges        = 100
	// maxSSELine and maxStream bound what a stream may send: a line of a change event is one Kubernetes object.
	maxSSELine = 4 << 20
	maxStream  = 32 << 20
)

func statusOf(o map[string]any) string {
	conds, _ := obj(o["status"])["conditions"].([]any)
	var c map[string]any
	for _, x := range conds {
		if m := obj(x); m["type"] == "Succeeded" {
			c = m
			break
		}
	}
	if c == nil {
		return "pending"
	}
	switch c["status"] {
	case "True":
		return "succeeded"
	case "False":
		if r, _ := c["reason"].(string); strings.Contains(r, "Cancel") {
			return "cancelled"
		}
		return "failed"
	}
	return "running"
}

func meta(o map[string]any) map[string]any { return obj(o["metadata"]) }

func succeededCond(st map[string]any) map[string]any {
	conds, _ := st["conditions"].([]any)
	for _, x := range conds {
		if m := obj(x); m["type"] == "Succeeded" {
			return m
		}
	}
	return map[string]any{}
}

func firstN(v any, n int) any {
	if s, ok := v.(string); ok {
		return clipRunes(s, n)
	}
	return v
}

func taskView(tr map[string]any) map[string]any {
	st := obj(tr["status"])
	steps := []any{}
	for _, x := range anyList(st["steps"]) {
		s := obj(x)
		state := "waiting"
		if _, ok := s["running"]; ok {
			state = "running"
		} else if _, ok := s["terminated"]; ok {
			state = "terminated"
		}
		step := map[string]any{"name": s["name"], "container": s["container"], "state": state}
		if t, ok := s["terminated"]; ok {
			step["exitCode"] = obj(t)["exitCode"]
		}
		steps = append(steps, step)
	}
	md := meta(tr)
	name := obj(md["labels"])[labelPipelineTask]
	if !pyTruthy(name) {
		name = md["name"]
	}
	out := map[string]any{"name": name, "taskRun": md["name"], "status": statusOf(tr), "steps": steps}
	for _, k := range []string{"startTime", "completionTime"} {
		if pyTruthy(st[k]) {
			out[k] = st[k]
		}
	}
	if s := out["status"]; s == "failed" || s == "cancelled" {
		if m := succeededCond(st)["message"]; pyTruthy(m) {
			out["message"] = firstN(m, 500)
		}
	}
	return out
}

func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

// sortedBy is a stable sort of items by a string key, ascending or descending (Python's sort(reverse=True) keeps the order
// of equal keys, as this does).
func sortedBy(items []map[string]any, key func(map[string]any) string, desc bool) {
	sort.SliceStable(items, func(i, j int) bool {
		if desc {
			return key(items[i]) > key(items[j])
		}
		return key(items[i]) < key(items[j])
	})
}

func timeKey(def string) func(map[string]any) string {
	return func(m map[string]any) string {
		if s, ok := m["startTime"].(string); ok && s != "" {
			return s
		}
		return def
	}
}

func runView(pr map[string]any, taskRuns []map[string]any) map[string]any {
	md, st := meta(pr), obj(pr["status"])
	labels := obj(md["labels"])
	cond := succeededCond(st)
	views := make([]map[string]any, 0, len(taskRuns))
	for _, t := range taskRuns {
		views = append(views, taskView(t))
	}
	sortedBy(views, timeKey("~"), false)
	tasks := make([]any, len(views))
	for i, v := range views {
		tasks[i] = v
	}
	out := map[string]any{"name": md["name"], "pipeline": labels[labelPipeline], "status": statusOf(pr), "reason": cond["reason"], "tasks": tasks}
	for _, k := range []string{"startTime", "completionTime"} {
		if pyTruthy(st[k]) {
			out[k] = st[k]
		}
	}
	for _, k := range []string{"pipelinesascode.tekton.dev/event-type", "pipelinesascode.tekton.dev/sha"} {
		if pyTruthy(labels[k]) {
			out[k[strings.Index(k, "/")+1:]] = labels[k]
		}
	}
	if s := out["status"]; s == "failed" || s == "cancelled" {
		if m := cond["message"]; pyTruthy(m) {
			out["message"] = firstN(m, 500)
		}
	}
	return out
}

// summarize is glidepath_live.summarize: GET /v1/apps/{app}/live as an agent reads it, each PipelineRun with its TaskRuns,
// running first, newest first.
func summarize(live map[string]any) map[string]any {
	byRun := map[string][]map[string]any{}
	for _, x := range anyList(live["taskRuns"]) {
		tr := obj(x)
		k, _ := obj(meta(tr)["labels"])[labelPipelineRun].(string)
		byRun[k] = append(byRun[k], tr)
	}
	var runs []map[string]any
	for _, x := range anyList(live["pipelineRuns"]) {
		pr := obj(x)
		name, _ := meta(pr)["name"].(string)
		runs = append(runs, runView(pr, byRun[name]))
	}
	sortedBy(runs, timeKey(""), true)
	rank := func(m map[string]any) string {
		if s := m["status"]; s == "running" || s == "pending" {
			return "0"
		}
		return "1"
	}
	sortedBy(runs, rank, false)
	out := make([]any, len(runs))
	for i, r := range runs {
		out[i] = r
	}
	return map[string]any{"pipelineRuns": out}
}

// Live is glidepath.live.list: GET /v1/apps/{app}/live summarised.
func (g *Glidepath) Live(ctx context.Context, app any, obo string) (any, error) {
	p, err := appPath(app, "/live")
	if err != nil {
		return nil, err
	}
	v, err := g.getJSON(ctx, gpReq{path: p, obo: obo})
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &dispatchKind{"AttributeError", "the live answer is not an object"}
	}
	if _, isErr := m["error"]; isErr {
		return m, nil
	}
	return summarize(m), nil
}

// LiveLog is glidepath.live.logs.get: a running TaskRun's step log, its last 256 KiB.
func (g *Glidepath) LiveLog(ctx context.Context, app, taskRun, container any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	t, err := seg("taskRun", taskRun)
	if err != nil {
		return nil, err
	}
	c, err := seg("container", container)
	if err != nil {
		return nil, err
	}
	body, other, err := g.getText(ctx, gpReq{path: "/v1/apps/" + a + "/live/taskruns/" + t + "/logs/" + c, obo: obo}, "text/plain")
	if err != nil || body == nil {
		return other, err
	}
	return tail(body, "log"), nil
}

type dispatchKind struct{ kind, msg string }

func (e *dispatchKind) Error() string { return e.kind + ": " + e.msg }
func (e *dispatchKind) Kind() string  { return e.kind }

// -- the stream -------------------------------------------------------------------------------------------------------

type sseReader struct {
	r     *bufio.Reader
	total int
}

// line is one line of the stream without its line end; ok is false at the end, err is a failure that is not the end.
func (s *sseReader) line() (string, bool, error) {
	var buf []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		buf = append(buf, chunk...)
		s.total += len(chunk)
		if len(buf) > maxSSELine || s.total > maxStream {
			return "", false, errors.New("the stream is larger than the gateway reads")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return "", false, nil // a last line with no end is dropped, as Python's event assembly drops an unfinished event
		}
		if err != nil {
			return "", false, err
		}
		return strings.TrimRight(decodeUTF8Replace(buf), "\r\n"), true, nil
	}
}

type sseEvent struct{ name, data string }

// next is sse_events: the next (event, data), and ("comment", text) for a comment line (glidepath-api's 15 s heartbeat is how a
// quiet stream still lets the caller look at its clock).
func (s *sseReader) next() (sseEvent, bool, error) {
	event := "message"
	var data []string
	trimL := func(x string) string {
		return strings.TrimLeftFunc(x, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
	}
	for {
		line, ok, err := s.line()
		if err != nil || !ok {
			return sseEvent{}, false, err
		}
		switch {
		case line == "":
			if len(data) > 0 {
				return sseEvent{event, strings.Join(data, "\n")}, true, nil
			}
			event, data = "message", nil
		case strings.HasPrefix(line, ":"):
			return sseEvent{"comment", pyStrip(line[1:])}, true, nil
		case strings.HasPrefix(line, "event:"):
			event = pyStrip(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, trimL(line[5:]))
		}
	}
}

// watchState is glidepath_live.Watch: one PipelineRun and its TaskRuns as a stream (or polling) reports them.
type watchState struct {
	name    string
	run     map[string]any
	hasRun  bool
	order   []string
	tasks   map[string]map[string]any
	changes []any
}

func newWatch(name string) *watchState {
	return &watchState{name: name, tasks: map[string]map[string]any{}}
}

func (w *watchState) mine(tr map[string]any) bool {
	return obj(meta(tr)["labels"])[labelPipelineRun] == w.name
}

func (w *watchState) snapshot(live map[string]any, record bool) {
	w.run, w.hasRun = nil, false
	for _, x := range anyList(live["pipelineRuns"]) {
		if p := obj(x); meta(p)["name"] == w.name {
			w.run, w.hasRun = p, true
			break
		}
	}
	for _, x := range anyList(live["taskRuns"]) {
		if tr := obj(x); w.mine(tr) {
			w.upsert(tr, record)
		}
	}
}

func (w *watchState) upsert(tr map[string]any, record bool) {
	name, _ := meta(tr)["name"].(string)
	before := ""
	if old, ok := w.tasks[name]; ok {
		before = statusOf(old)
	} else {
		w.order = append(w.order, name)
	}
	w.tasks[name] = tr
	if record && statusOf(tr) != before && len(w.changes) < maxChanges {
		t := taskView(tr)
		w.changes = append(w.changes, map[string]any{"at": time.Now().UTC().Format("2006-01-02T15:04:05Z"), "task": t["name"], "taskRun": name, "status": t["status"]})
	}
}

func (w *watchState) change(ev map[string]any) {
	o, kind := obj(ev["object"]), ev["kind"]
	switch {
	case kind == "PipelineRun" && ev["name"] == w.name:
		if ev["type"] == "delete" {
			w.run, w.hasRun = nil, false
		} else {
			w.run, w.hasRun = o, true
		}
	case kind == "TaskRun" && ev["type"] == "upsert" && w.mine(o):
		w.upsert(o, true)
	}
}

func (w *watchState) finished() bool {
	if !w.hasRun {
		return false
	}
	s := statusOf(w.run)
	return s != "running" && s != "pending"
}

func (w *watchState) answer(waited float64, finished bool, via string) map[string]any {
	changes := w.changes
	if changes == nil {
		changes = []any{}
	}
	out := map[string]any{"finished": finished, "waitedSeconds": pyFloat(math.RoundToEven(waited*10) / 10), "via": via, "changes": changes}
	if w.hasRun && len(w.run) > 0 {
		var trs []map[string]any
		for _, n := range w.order {
			trs = append(trs, w.tasks[n])
		}
		out["pipelineRun"] = runView(w.run, trs)
	} else {
		out["pipelineRun"] = nil
	}
	return out
}

// pyFloat is a float as Python's JSON writes it (3.0, 0.1).
func pyFloat(f float64) jsonNumber { return jsonNumber(pyFloatRepr(f)) }

type jsonNumber = json.Number

// Watch is glidepath.live.watch: follow PipelineRun `name` until it finishes or timeoutSeconds (1 to 120, default 60) pass.
// A run that is not in the cluster is an answer (`pipelineRun: null`), not an error: it may have finished and been deleted,
// which glidepath.runs.list then shows. The wait is cut short to leave the call's own deadline two seconds.
func (g *Glidepath) Watch(ctx context.Context, app, pipelinerun, timeoutSeconds any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	name, err := argStr("pipelinerun", pipelinerun)
	if err != nil {
		return nil, err
	}
	secs := int64(watchDef)
	if n, ok, err := optInt("timeoutSeconds", timeoutSeconds, 1, watchMax); err != nil {
		return nil, err
	} else if ok {
		secs = n
	}
	timeout := float64(secs)
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl).Seconds() - 2; left < timeout {
			timeout = math.Max(left, 1)
		}
	}
	path := "/v1/apps/" + a + "/live/stream"
	poll := func() (any, error) { return g.getJSON(ctx, gpReq{path: "/v1/apps/" + a + "/live", obo: obo}) }

	start, w := g.clock(), newWatch(name)
	status, body, src, release := g.openStream(ctx, gpReq{method: http.MethodGet, path: path, accept: "text/event-stream", obo: obo}, timeout)
	defer release()
	if status == 401 || status == 403 || status == 404 || status == 400 {
		return refusal(status, body, hint422)
	}
	via := "stream"
	if status == 200 {
		g.readStream(src, w, start, timeout)
		if !w.hasRun || w.finished() || g.clock()-start >= timeout {
			return w.answer(g.clock()-start, w.finished(), via), nil
		}
	}
	via = "poll"
	if status == 200 {
		via = "stream+poll"
	}
	first := true
	for {
		live, err := poll()
		if err != nil {
			return nil, err
		}
		m, isMap := live.(map[string]any)
		if !isMap {
			return nil, &dispatchKind{"AttributeError", "the live answer is not an object"}
		}
		if _, isErr := m["error"]; isErr {
			return m, nil
		}
		w.snapshot(m, !first)
		first = false
		if !w.hasRun || w.finished() || g.clock()-start >= timeout {
			return w.answer(g.clock()-start, w.finished(), via), nil
		}
		wait := math.Min(pollEvery.Seconds(), math.Max(0, timeout-(g.clock()-start)))
		if !g.sleep(ctx, time.Duration(wait*float64(time.Second))) {
			return nil, ctx.Err()
		}
	}
}

// readStream is the stream half of glidepath_live.watch. A stream that drops, times out or sends what is not JSON ends the
// reading; the caller then polls for the rest of the deadline.
func (g *Glidepath) readStream(src *sseReader, w *watchState, start, timeout float64) {
	if src == nil {
		return
	}
	for {
		ev, ok, err := src.next()
		if err != nil || !ok {
			return
		}
		if ev.name == "comment" {
			if ev.data == "lagged" || g.clock()-start >= timeout {
				return
			}
			continue
		}
		var body map[string]any
		v, derr := decodeJSON([]byte(ev.data))
		if derr != nil {
			return
		}
		if body, _ = v.(map[string]any); body == nil {
			return
		}
		switch ev.name {
		case "snapshot":
			w.snapshot(body, false)
			if !w.hasRun || w.finished() {
				return
			}
		case "change":
			w.change(body)
			if !w.hasRun || w.finished() {
				return
			}
		}
		if g.clock()-start >= timeout {
			return
		}
	}
}

// openStream opens the event stream under its own limiter and a deadline of the watch's length: (200, nil, a reader) when it
// opens, else (the status, the error body, nil), or 599 when it cannot be opened at all (the watch then polls). The reader
// is cancelled when nothing arrives for 30 s, as Python's socket timeout does. release must be called when done.
func (g *Glidepath) openStream(ctx context.Context, r gpReq, timeout float64) (int, []byte, *sseReader, func()) {
	noop := func() {}
	if err := g.streamLim.acquire(ctx); err != nil {
		return 599, nil, nil, noop
	}
	sctx, cancel := context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
	var once sync.Once
	release := func() { once.Do(func() { cancel(); g.streamLim.release() }) }
	full, err := g.url(r)
	if err != nil {
		release()
		return 599, nil, nil, noop
	}
	h, tok, err := g.headers(r)
	if err != nil {
		release()
		return 599, nil, nil, noop
	}
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, full, nil)
	if err != nil {
		release()
		return 599, nil, nil, noop
	}
	for k, v := range h {
		req.Header[http.CanonicalHeaderKey(k)] = v
	}
	resp, err := g.streamHC.Do(req)
	if err != nil {
		release()
		return 599, nil, nil, noop
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxGPErr))
		resp.Body.Close()
		release()
		return resp.StatusCode, redact(b, tok), nil, noop
	}
	idle := time.AfterFunc(streamIdle, cancel)
	rd := &idleReader{r: resp.Body, idle: idle}
	return 200, nil, &sseReader{r: bufio.NewReaderSize(rd, 64<<10)}, func() { idle.Stop(); resp.Body.Close(); release() }
}

type idleReader struct {
	r    io.Reader
	idle *time.Timer
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.idle.Reset(streamIdle)
	}
	return n, err
}
