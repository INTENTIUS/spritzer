package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/intentius/spritzer/internal/clock"
)

type harness struct {
	t  *testing.T
	ts *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := New(Options{Version: "test", Clock: clock.NewFake(time.Time{})})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, ts: ts}
}

// do performs a request and returns status and raw body.
func (h *harness) do(method, path string, body any) (int, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, reader)
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

func (h *harness) mustJSON(raw []byte, dst any) {
	h.t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		h.t.Fatalf("unmarshal %s: %v", raw, err)
	}
}

func TestHealth(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(http.MethodGet, "/_spritzer/health", nil)
	if code != http.StatusOK {
		t.Fatalf("health status = %d", code)
	}
	var payload struct {
		Status      string   `json:"status"`
		Version     string   `json:"version"`
		Implemented []string `json:"implemented"`
	}
	h.mustJSON(body, &payload)
	if payload.Status != "ok" || payload.Version != "test" {
		t.Fatalf("unexpected health payload: %+v", payload)
	}
	if len(payload.Implemented) == 0 {
		t.Fatalf("expected non-empty coverage list: %+v", payload)
	}
}

func TestCreateRequiresName(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{})
	if code != http.StatusBadRequest {
		t.Fatalf("create without name = %d %s, want 400", code, body)
	}
	var e ErrorResponse
	h.mustJSON(body, &e)
	if e.Error != "name is required" || e.Status != http.StatusBadRequest {
		t.Fatalf("unexpected error envelope: %+v", e)
	}
}

func TestCreateResponseShape(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "task-1", "image": "ubuntu"})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", code, body)
	}
	var resp createResponse
	h.mustJSON(body, &resp)
	if resp.ID != "task-1" {
		t.Fatalf("id = %q, want task-1", resp.ID)
	}
	// url = http://<host>/s/<id>
	if resp.URL == "" || resp.URL[:7] != "http://" {
		t.Fatalf("url = %q, want http://<host>/s/task-1", resp.URL)
	}
}

// TestFullLoop exercises every endpoint: create, exec write, checkpoint, exec
// corrupt, GET (corrupt), restore, GET (rewound), destroy, then 404s.
func TestFullLoop(t *testing.T) {
	h := newHarness(t)

	// create
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s1"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}

	// exec: write a key
	code, body := h.do(http.MethodPost, "/v1/sprites/s1/exec", map[string]any{"cmd": "echo good > /state"})
	if code != http.StatusOK {
		t.Fatalf("exec write = %d %s", code, body)
	}
	var ex struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exitCode"`
	}
	h.mustJSON(body, &ex)
	if ex.ExitCode != 0 {
		t.Fatalf("exec write exitCode = %d, want 0", ex.ExitCode)
	}

	// checkpoint: the server assigns the version id v1; the caller supplies a
	// comment.
	code, body = h.do(http.MethodPost, "/v1/sprites/s1/checkpoints", map[string]any{"comment": "pre-run"})
	if code != http.StatusCreated {
		t.Fatalf("checkpoint = %d %s", code, body)
	}
	var cp checkpointResponse
	h.mustJSON(body, &cp)
	if cp.ID != "v1" {
		t.Fatalf("checkpoint id = %q, want v1", cp.ID)
	}

	// list checkpoints reports v1 with its comment
	code, body = h.do(http.MethodGet, "/v1/sprites/s1/checkpoints", nil)
	if code != http.StatusOK {
		t.Fatalf("list checkpoints = %d %s", code, body)
	}
	var list listCheckpointsResponse
	h.mustJSON(body, &list)
	if len(list.Checkpoints) != 1 || list.Checkpoints[0].ID != "v1" || list.Checkpoints[0].Comment != "pre-run" {
		t.Fatalf("list = %+v, want [{v1 pre-run}]", list.Checkpoints)
	}

	// exec: corrupt via risky.sh (exit 1) — the server still returns 200 with
	// the exec result; a non-zero exit is the client's concern.
	code, body = h.do(http.MethodPost, "/v1/sprites/s1/exec", map[string]any{"cmd": "echo bad > /state; ./risky.sh"})
	if code != http.StatusOK {
		t.Fatalf("exec corrupt = %d %s", code, body)
	}
	h.mustJSON(body, &ex)
	if ex.ExitCode != 1 || ex.Stderr != "risky.sh: failed\n" {
		t.Fatalf("exec corrupt = %+v, want exit 1 + stderr", ex)
	}

	// GET shows corruption
	code, body = h.do(http.MethodGet, "/v1/sprites/s1", nil)
	if code != http.StatusOK {
		t.Fatalf("get corrupt = %d %s", code, body)
	}
	var view struct {
		ID          string            `json:"id"`
		Status      string            `json:"status"`
		URL         string            `json:"url"`
		FS          map[string]string `json:"fs"`
		Checkpoints []struct {
			ID      string `json:"id"`
			Comment string `json:"comment"`
		} `json:"checkpoints"`
	}
	h.mustJSON(body, &view)
	if view.FS["/state"] != "bad" || view.FS["/work/output"] != "partial-corrupt" {
		t.Fatalf("fs before restore = %v, want corrupt", view.FS)
	}
	if len(view.Checkpoints) != 1 || view.Checkpoints[0].ID != "v1" || view.Checkpoints[0].Comment != "pre-run" {
		t.Fatalf("checkpoints = %+v, want [{v1 pre-run}]", view.Checkpoints)
	}

	// restore by id in the path
	if code, body := h.do(http.MethodPost, "/v1/sprites/s1/checkpoints/v1/restore", nil); code != http.StatusOK {
		t.Fatalf("restore = %d %s", code, body)
	}

	// GET shows rewound. Use a fresh struct: unmarshaling into the populated
	// `view` above would merge maps and not clear /work/output.
	var rewound struct {
		Status string            `json:"status"`
		FS     map[string]string `json:"fs"`
	}
	_, body = h.do(http.MethodGet, "/v1/sprites/s1", nil)
	h.mustJSON(body, &rewound)
	if len(rewound.FS) != 1 || rewound.FS["/state"] != "good" {
		t.Fatalf("fs after restore = %v, want {/state: good}", rewound.FS)
	}
	if rewound.Status != "running" {
		t.Fatalf("status after restore = %q, want running", rewound.Status)
	}

	// destroy
	if code, body := h.do(http.MethodDelete, "/v1/sprites/s1", nil); code != http.StatusOK {
		t.Fatalf("destroy = %d %s", code, body)
	}

	// every op past destroy is 404
	if code, _ := h.do(http.MethodGet, "/v1/sprites/s1", nil); code != http.StatusNotFound {
		t.Fatalf("get after destroy = %d, want 404", code)
	}
	if code, _ := h.do(http.MethodPost, "/v1/sprites/s1/exec", map[string]any{"cmd": "true"}); code != http.StatusNotFound {
		t.Fatalf("exec after destroy = %d, want 404", code)
	}
	if code, _ := h.do(http.MethodDelete, "/v1/sprites/s1", nil); code != http.StatusNotFound {
		t.Fatalf("destroy after destroy = %d, want 404", code)
	}
}

// TestCheckpointVersionIDOverHTTP confirms the server assigns v1 for the first
// checkpoint even when the body carries no comment.
func TestCheckpointVersionIDOverHTTP(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	code, body := h.do(http.MethodPost, "/v1/sprites/s/checkpoints", map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("checkpoint = %d %s", code, body)
	}
	var cp checkpointResponse
	h.mustJSON(body, &cp)
	if cp.ID != "v1" {
		t.Fatalf("checkpoint id = %q, want v1", cp.ID)
	}
}

// TestListCheckpointsEmpty confirms a sprite with no checkpoints lists as an
// empty array, not null.
func TestListCheckpointsEmpty(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	code, body := h.do(http.MethodGet, "/v1/sprites/s/checkpoints", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	if s := string(body); !strings.Contains(s, `"checkpoints":[]`) {
		t.Fatalf("empty list body = %s, want checkpoints:[]", s)
	}
}

// TestRestoreUnknownCheckpoint404 confirms an unknown id in the path is a 404.
func TestRestoreUnknownCheckpoint404(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	code, body := h.do(http.MethodPost, "/v1/sprites/s/checkpoints/v99/restore", nil)
	if code != http.StatusNotFound {
		t.Fatalf("restore unknown = %d %s, want 404", code, body)
	}
	var e ErrorResponse
	h.mustJSON(body, &e)
	if e.Status != http.StatusNotFound {
		t.Fatalf("error status = %d, want 404", e.Status)
	}
}

// TestOpsOnMissingSprite confirms a missing sprite is 404 on exec/get/destroy.
func TestOpsOnMissingSprite(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/sprites/ghost", nil},
		{http.MethodPost, "/v1/sprites/ghost/exec", map[string]any{"cmd": "true"}},
		{http.MethodPost, "/v1/sprites/ghost/checkpoints", map[string]any{}},
		{http.MethodGet, "/v1/sprites/ghost/checkpoints", nil},
		{http.MethodPost, "/v1/sprites/ghost/checkpoints/v1/restore", nil},
		{http.MethodDelete, "/v1/sprites/ghost", nil},
	} {
		if code, body := h.do(tc.method, tc.path, tc.body); code != http.StatusNotFound {
			t.Fatalf("%s %s = %d %s, want 404", tc.method, tc.path, code, body)
		}
	}
}

// TestEmptyFSMarshalsAsObject confirms a fresh sprite's fs is {} and checkpoints
// is [] (not null), matching the fake's JSON shape.
func TestEmptyFSMarshalsAsObject(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	_, body := h.do(http.MethodGet, "/v1/sprites/s", nil)
	if s := string(body); !strings.Contains(s, `"fs":{}`) || !strings.Contains(s, `"checkpoints":[]`) {
		t.Fatalf("empty sprite GET body = %s, want fs:{} and checkpoints:[]", s)
	}
}
