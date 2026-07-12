package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/intentius/spritzer/internal/clock"
	"github.com/intentius/spritzer/internal/sprite"
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

// checkpointID posts to the singular /checkpoint endpoint, scans the NDJSON
// progress stream, and returns the id from the terminal complete event.
func (h *harness) checkpointID(name, comment string) string {
	h.t.Helper()
	body := map[string]any{}
	if comment != "" {
		body["comment"] = comment
	}
	code, raw := h.do(http.MethodPost, "/v1/sprites/"+name+"/checkpoint", body)
	if code != http.StatusOK {
		h.t.Fatalf("checkpoint = %d %s, want 200", code, raw)
	}
	return completeID(h.t, raw)
}

// idFromData mines the version id out of a progress message, mirroring how a
// real client reads it: the "  ID: v1" detail line and the "Checkpoint v1
// created successfully" completion line both carry it.
var idFromData = regexp.MustCompile(`(?:(?:^|\s)ID:\s*|Checkpoint\s+)(\S+)`)

// completeID scans an NDJSON progress body and returns the version id mined from
// the message text, requiring a terminal {"type":"complete"} line.
func completeID(t *testing.T, raw []byte) string {
	t.Helper()
	var id string
	sawComplete := false
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev progressEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("ndjson line %q: %v", line, err)
		}
		if m := idFromData.FindStringSubmatch(ev.Data); m != nil {
			id = m[1]
		}
		if ev.Type == "complete" {
			sawComplete = true
		}
	}
	if !sawComplete {
		t.Fatalf("ndjson stream had no complete event: %s", raw)
	}
	return id
}

// execWS connects the control WebSocket, sends the command as a single cmd
// query param, reads the framed stdout/stderr/exit, and returns them. It fails
// the test if no exit frame arrives.
func (h *harness) execWS(name, cmd string) (stdout, stderr string, exit int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	q := url.Values{"cmd": {cmd}, "stdin": {"false"}}
	wsURL := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/v1/sprites/" + name + "/exec?" + q.Encode()

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		h.t.Fatalf("ws dial %s: %v", name, err)
	}
	defer func() { _ = c.CloseNow() }()

	sawExit := false
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			continue
		}
		switch data[0] {
		case streamStdout:
			stdout += string(data[1:])
		case streamStderr:
			stderr += string(data[1:])
		case streamExit:
			if len(data) >= 2 {
				exit = int(data[1])
			}
			sawExit = true
		}
		if sawExit {
			break
		}
	}
	if !sawExit {
		h.t.Fatalf("exec ws %q: no exit frame", cmd)
	}
	return stdout, stderr, exit
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
	// The exec entry names the control WebSocket, and checkpoint create is the
	// singular path.
	joined := strings.Join(payload.Implemented, "\n")
	if !strings.Contains(joined, "exec (control WebSocket)") {
		t.Fatalf("coverage list missing WS exec entry: %v", payload.Implemented)
	}
	if !strings.Contains(joined, "POST /v1/sprites/{id}/checkpoint\n") && !strings.HasSuffix(joined, "POST /v1/sprites/{id}/checkpoint") {
		t.Fatalf("coverage list missing singular checkpoint create: %v", payload.Implemented)
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

// TestFullLoop exercises the whole surface: create, WS exec write, checkpoint
// (NDJSON), WS exec corrupt, GET (corrupt), restore (NDJSON), GET (rewound),
// destroy, then 404s.
func TestFullLoop(t *testing.T) {
	h := newHarness(t)

	// create
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s1"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}

	// exec over the control WebSocket: write a key, exit frame 0.
	if stdout, stderr, exit := h.execWS("s1", "echo good > /state"); exit != 0 || stdout != "" || stderr != "" {
		t.Fatalf("exec write = out=%q err=%q exit=%d, want exit 0", stdout, stderr, exit)
	}

	// checkpoint: the server assigns v1; the caller supplies a comment.
	if id := h.checkpointID("s1", "pre-run"); id != "v1" {
		t.Fatalf("checkpoint id = %q, want v1", id)
	}

	// list checkpoints is a bare array with is_auto false and a create_time.
	code, body := h.do(http.MethodGet, "/v1/sprites/s1/checkpoints", nil)
	if code != http.StatusOK {
		t.Fatalf("list checkpoints = %d %s", code, body)
	}
	var list []sprite.CheckpointInfo
	h.mustJSON(body, &list)
	if len(list) != 1 || list[0].ID != "v1" || list[0].Comment != "pre-run" || list[0].IsAuto {
		t.Fatalf("list = %+v, want [{v1 pre-run is_auto:false}]", list)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		t.Fatalf("list body = %s, want a bare JSON array", body)
	}

	// get a single checkpoint
	code, body = h.do(http.MethodGet, "/v1/sprites/s1/checkpoints/v1", nil)
	if code != http.StatusOK {
		t.Fatalf("get checkpoint = %d %s", code, body)
	}
	var one sprite.CheckpointInfo
	h.mustJSON(body, &one)
	if one.ID != "v1" || one.Comment != "pre-run" || one.IsAuto {
		t.Fatalf("get checkpoint = %+v, want {v1 pre-run is_auto:false}", one)
	}

	// exec: corrupt via risky.sh (stderr + exit frame 1).
	stdout, stderr, exit := h.execWS("s1", "echo bad > /state; ./risky.sh")
	if exit != 1 || stderr != "risky.sh: failed\n" || stdout != "" {
		t.Fatalf("exec corrupt = out=%q err=%q exit=%d, want exit 1 + stderr", stdout, stderr, exit)
	}

	// GET shows corruption
	code, body = h.do(http.MethodGet, "/v1/sprites/s1", nil)
	if code != http.StatusOK {
		t.Fatalf("get corrupt = %d %s", code, body)
	}
	var view struct {
		ID          string                  `json:"id"`
		Status      string                  `json:"status"`
		URL         string                  `json:"url"`
		FS          map[string]string       `json:"fs"`
		Checkpoints []sprite.CheckpointInfo `json:"checkpoints"`
	}
	h.mustJSON(body, &view)
	if view.FS["/state"] != "bad" || view.FS["/work/output"] != "partial-corrupt" {
		t.Fatalf("fs before restore = %v, want corrupt", view.FS)
	}
	if len(view.Checkpoints) != 1 || view.Checkpoints[0].ID != "v1" || view.Checkpoints[0].Comment != "pre-run" {
		t.Fatalf("checkpoints = %+v, want [{v1 pre-run}]", view.Checkpoints)
	}

	// restore by id in the path: consume the NDJSON stream and confirm it names v1.
	code, body = h.do(http.MethodPost, "/v1/sprites/s1/checkpoints/v1/restore", nil)
	if code != http.StatusOK {
		t.Fatalf("restore = %d %s", code, body)
	}
	if id := completeID(t, body); id != "v1" {
		t.Fatalf("restore complete id = %q, want v1", id)
	}

	// GET shows rewound. Use a fresh struct so maps do not merge.
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
	if code, _ := h.do(http.MethodPost, "/v1/sprites/s1/checkpoint", map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("checkpoint after destroy = %d, want 404", code)
	}
	if code, _ := h.do(http.MethodDelete, "/v1/sprites/s1", nil); code != http.StatusNotFound {
		t.Fatalf("destroy after destroy = %d, want 404", code)
	}
}

// TestExecWSFrames asserts the framing directly: echo writes fs and exits 0;
// risky.sh yields a stderr frame and exit 1.
func TestExecWSFrames(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "ws"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}

	// echo hi (no redirect) -> stdout "hi\n", exit 0.
	if stdout, stderr, exit := h.execWS("ws", "echo hi"); stdout != "hi\n" || stderr != "" || exit != 0 {
		t.Fatalf("echo hi = out=%q err=%q exit=%d, want out=\"hi\\n\" exit 0", stdout, stderr, exit)
	}

	// echo hi > /state writes the fs and exits 0.
	if _, _, exit := h.execWS("ws", "echo hi > /state"); exit != 0 {
		t.Fatalf("echo redirect exit = %d, want 0", exit)
	}
	_, body := h.do(http.MethodGet, "/v1/sprites/ws", nil)
	var view struct {
		FS map[string]string `json:"fs"`
	}
	h.mustJSON(body, &view)
	if view.FS["/state"] != "hi" {
		t.Fatalf("fs after ws write = %v, want {/state: hi}", view.FS)
	}

	// risky.sh -> stderr frame + exit 1.
	if stdout, stderr, exit := h.execWS("ws", "./risky.sh"); exit != 1 || stderr != "risky.sh: failed\n" || stdout != "" {
		t.Fatalf("risky = out=%q err=%q exit=%d, want stderr + exit 1", stdout, stderr, exit)
	}
}

// TestExecWSArgvParams confirms repeated cmd params are joined into the command
// line (argv form), equivalent to a single cmd param.
func TestExecWSArgvParams(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "argv"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Send `echo` `hi` as separate argv elements plus path=echo.
	q := url.Values{"cmd": {"echo", "hi"}, "path": {"echo"}, "stdin": {"false"}}
	wsURL := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/v1/sprites/argv/exec?" + q.Encode()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	var stdout string
	var exit int
	sawExit := false
	for !sawExit {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			continue
		}
		switch data[0] {
		case streamStdout:
			stdout += string(data[1:])
		case streamExit:
			if len(data) >= 2 {
				exit = int(data[1])
			}
			sawExit = true
		}
	}
	if stdout != "hi\n" || exit != 0 {
		t.Fatalf("argv exec = out=%q exit=%d, want out=\"hi\\n\" exit 0", stdout, exit)
	}
}

// TestExecWSMissingSprite confirms the WS closes with a policy-violation status
// for an unknown sprite.
func TestExecWSMissingSprite(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q := url.Values{"cmd": {"true"}, "stdin": {"false"}}
	wsURL := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/v1/sprites/ghost/exec?" + q.Encode()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	// A read should fail with a close status carrying the policy violation.
	_, _, readErr := c.Read(ctx)
	if readErr == nil {
		t.Fatalf("expected close for missing sprite, got a frame")
	}
	if status := websocket.CloseStatus(readErr); status != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v, want policy violation", status)
	}
}

// TestCheckpointVersionIDOverHTTP confirms the server assigns v1 for the first
// checkpoint even when the body carries no comment, via the NDJSON stream.
func TestCheckpointVersionIDOverHTTP(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	if id := h.checkpointID("s", ""); id != "v1" {
		t.Fatalf("checkpoint id = %q, want v1", id)
	}
}

// TestListCheckpointsEmpty confirms a sprite with no checkpoints lists as an
// empty bare array, not null and not a wrapper object.
func TestListCheckpointsEmpty(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	code, body := h.do(http.MethodGet, "/v1/sprites/s/checkpoints", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	if s := strings.TrimSpace(string(body)); s != "[]" {
		t.Fatalf("empty list body = %s, want []", s)
	}
}

// TestRestoreUnknownCheckpoint404 confirms an unknown id in the path is a 404
// (JSON error, not an NDJSON stream).
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

// TestGetUnknownCheckpoint404 confirms GET of an unknown checkpoint id is a 404.
func TestGetUnknownCheckpoint404(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(http.MethodPost, "/v1/sprites", map[string]any{"name": "s"}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	code, body := h.do(http.MethodGet, "/v1/sprites/s/checkpoints/v99", nil)
	if code != http.StatusNotFound {
		t.Fatalf("get unknown checkpoint = %d %s, want 404", code, body)
	}
}

// TestOpsOnMissingSprite confirms a missing sprite is 404 on the HTTP ops.
func TestOpsOnMissingSprite(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/sprites/ghost", nil},
		{http.MethodPost, "/v1/sprites/ghost/checkpoint", map[string]any{}},
		{http.MethodGet, "/v1/sprites/ghost/checkpoints", nil},
		{http.MethodGet, "/v1/sprites/ghost/checkpoints/v1", nil},
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
