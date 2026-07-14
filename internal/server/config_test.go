package server

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/intentius/spritzer/internal/sprite"
)

// doRaw performs a request with a raw (non-JSON) string body and returns the
// status and raw response bytes — for the filesystem read/write endpoints.
func (h *harness) doRaw(method, path, body string) (int, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (h *harness) createSprite(name string) {
	h.t.Helper()
	if st, body := h.do("POST", "/v1/sprites", map[string]any{"name": name}); st != http.StatusCreated {
		h.t.Fatalf("create %s: status %d: %s", name, st, body)
	}
}

func TestFilesystem(t *testing.T) {
	h := newHarness(t)
	h.createSprite("fs1")

	if st, body := h.doRaw("PUT", "/v1/sprites/fs1/fs/write?path=/work/input", "hello"); st != http.StatusOK {
		t.Fatalf("write: %d %s", st, body)
	}
	if st, body := h.doRaw("GET", "/v1/sprites/fs1/fs/read?path=/work/input", ""); st != http.StatusOK || string(body) != "hello" {
		t.Fatalf("read: %d %q", st, body)
	}
	// A missing file reads as 404.
	if st, _ := h.doRaw("GET", "/v1/sprites/fs1/fs/read?path=/nope", ""); st != http.StatusNotFound {
		t.Fatalf("read missing: want 404, got %d", st)
	}

	// list shows immediate children: a nested key contributes its dir once.
	h.doRaw("PUT", "/v1/sprites/fs1/fs/write?path=/work/notes/a.txt", "x")
	var entries []sprite.DirEntry
	st, body := h.do("GET", "/v1/sprites/fs1/fs/list?path=/work", nil)
	if st != http.StatusOK {
		t.Fatalf("list: %d %s", st, body)
	}
	h.mustJSON(body, &entries)
	want := map[string]string{"notes": "dir", "input": "file"}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = e.Type
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Fatalf("list entries = %v, want %s=%s", entries, name, typ)
		}
	}

	// delete then confirm gone; a second delete is a 404 (idempotent for callers).
	if st, _ := h.do("DELETE", "/v1/sprites/fs1/fs/delete?path=/work/input", nil); st != http.StatusOK {
		t.Fatalf("delete: %d", st)
	}
	if st, _ := h.doRaw("GET", "/v1/sprites/fs1/fs/read?path=/work/input", ""); st != http.StatusNotFound {
		t.Fatalf("read after delete: want 404, got %d", st)
	}
	if st, _ := h.do("DELETE", "/v1/sprites/fs1/fs/delete?path=/work/input", nil); st != http.StatusNotFound {
		t.Fatalf("re-delete: want 404, got %d", st)
	}

	// recursive delete clears a subtree.
	if st, _ := h.do("DELETE", "/v1/sprites/fs1/fs/delete?path=/work&recursive=true", nil); st != http.StatusOK {
		t.Fatalf("recursive delete: %d", st)
	}
	h.do("GET", "/v1/sprites/fs1/fs/list?path=/work", nil)
}

func TestNetworkPolicy(t *testing.T) {
	h := newHarness(t)
	h.createSprite("np1")

	rules := map[string]any{"rules": []map[string]string{
		{"domain": "api.anthropic.com", "action": "allow"},
		{"domain": "*", "action": "deny"},
	}}
	if st, body := h.do("POST", "/v1/sprites/np1/policy/network", rules); st != http.StatusOK {
		t.Fatalf("set policy: %d %s", st, body)
	}
	var got policyBody
	_, body := h.do("GET", "/v1/sprites/np1/policy/network", nil)
	h.mustJSON(body, &got)
	if len(got.Rules) != 2 || got.Rules[0].Domain != "api.anthropic.com" || got.Rules[1].Action != "deny" {
		t.Fatalf("policy = %+v", got.Rules)
	}
}

func TestServices(t *testing.T) {
	h := newHarness(t)
	h.createSprite("svc1")

	svc := map[string]any{"cmd": "run-web", "http_port": 8080, "needs": []string{"db"}}
	if st, body := h.do("PUT", "/v1/sprites/svc1/services/web", svc); st != http.StatusOK {
		t.Fatalf("put service: %d %s", st, body)
	}
	// start flips the state to running (NDJSON stream).
	if st, _ := h.do("POST", "/v1/sprites/svc1/services/web/start", nil); st != http.StatusOK {
		t.Fatalf("start: %d", st)
	}
	var svcOut sprite.Service
	_, body := h.do("GET", "/v1/sprites/svc1/services/web", nil)
	h.mustJSON(body, &svcOut)
	if svcOut.State.Status != "running" || svcOut.HTTPPort != 8080 {
		t.Fatalf("service = %+v", svcOut)
	}
	// stop flips it back.
	h.do("POST", "/v1/sprites/svc1/services/web/stop", nil)
	_, body = h.do("GET", "/v1/sprites/svc1/services/web", nil)
	h.mustJSON(body, &svcOut)
	if svcOut.State.Status != "stopped" {
		t.Fatalf("after stop: %+v", svcOut.State)
	}
	// an unknown service is a 404.
	if st, _ := h.do("GET", "/v1/sprites/svc1/services/nope", nil); st != http.StatusNotFound {
		t.Fatalf("get missing service: want 404, got %d", st)
	}
}

func TestTasks(t *testing.T) {
	h := newHarness(t)
	h.createSprite("t1")

	if st, body := h.do("POST", "/v1/sprites/t1/tasks", map[string]any{"name": "session", "expire": "5m"}); st != http.StatusCreated {
		t.Fatalf("create task: %d %s", st, body)
	}
	var tasks []sprite.Task
	_, body := h.do("GET", "/v1/sprites/t1/tasks", nil)
	h.mustJSON(body, &tasks)
	if len(tasks) != 1 || tasks[0].Name != "session" {
		t.Fatalf("tasks = %+v", tasks)
	}
	// refresh an existing task, then release it; a second release is 404.
	if st, _ := h.do("PUT", "/v1/sprites/t1/tasks/session", map[string]any{"expire": "5m"}); st != http.StatusOK {
		t.Fatalf("refresh: %d", st)
	}
	if st, _ := h.do("DELETE", "/v1/sprites/t1/tasks/session", nil); st != http.StatusNoContent {
		t.Fatalf("release: %d", st)
	}
	if st, _ := h.do("DELETE", "/v1/sprites/t1/tasks/session", nil); st != http.StatusNotFound {
		t.Fatalf("re-release: want 404, got %d", st)
	}
	// refreshing a missing task is a 404.
	if st, _ := h.do("PUT", "/v1/sprites/t1/tasks/session", map[string]any{"expire": "1m"}); st != http.StatusNotFound {
		t.Fatalf("refresh missing: want 404, got %d", st)
	}
}

func TestConfigEndpointsInCoverage(t *testing.T) {
	h := newHarness(t)
	_, body := h.do("GET", "/_spritzer/health", nil)
	var health struct {
		Implemented []string `json:"implemented"`
	}
	h.mustJSON(body, &health)
	set := map[string]bool{}
	for _, p := range health.Implemented {
		set[p] = true
	}
	for _, want := range []string{
		"PUT /v1/sprites/{id}/fs/write",
		"POST /v1/sprites/{id}/policy/network",
		"PUT /v1/sprites/{id}/services/{svc}",
		"POST /v1/sprites/{id}/tasks",
	} {
		if !set[want] {
			t.Fatalf("health coverage missing %q", want)
		}
	}
}
