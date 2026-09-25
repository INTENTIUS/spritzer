//go:build !windows

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testPaths returns a root short enough for a unix socket path.
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir, err := os.MkdirTemp("", "sa")
	if err != nil {
		t.Fatal(err)
	}
	if len(dir) > 80 {
		if d, err := os.MkdirTemp("/tmp", "sa"); err == nil {
			_ = os.RemoveAll(dir)
			dir = d
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return Paths{Root: dir}
}

// serve starts the management API on a socket and returns a sprite-env client.
func serve(t *testing.T, p Paths) (*Supervisor, *envClient, *bytes.Buffer) {
	t.Helper()
	sv := NewSupervisor(p)
	ln, err := Listen(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: Handler(sv)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		sv.StopAll(2 * time.Second)
	})
	var out bytes.Buffer
	return sv, &envClient{sock: p.Socket(), out: &out}, &out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServiceLifecycleThroughSpriteEnv(t *testing.T) {
	p := testPaths(t)
	sv, c, out := serve(t, p)

	err := c.services("create", []string{"echoer", "--cmd", "/bin/sh", "--args", "-c,echo hello; exec sleep 30", "--http-port", "9090", "--duration", "500ms"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(out.String(), `"type":"started"`) || !strings.Contains(out.String(), `"data":"hello"`) {
		t.Fatalf("create stream: %s", out)
	}
	got, err := sv.Get("echoer")
	if err != nil || got.State.Status != "running" || got.State.PID == 0 {
		t.Fatalf("state after create: %+v %v", got.State, err)
	}
	if URLPort(p) != 9090 {
		t.Fatalf("URLPort = %d, want the service's http_port", URLPort(p))
	}
	b, _ := os.ReadFile(sv.LogPath("echoer"))
	if !strings.Contains(string(b), "[stdout] hello") {
		t.Fatalf("log: %q", b)
	}
	if _, err := os.Stat(filepath.Join(p.ServicesDir(), "echoer.json")); err != nil {
		t.Fatalf("definition not on disk: %v", err)
	}

	out.Reset()
	if err := c.services("stop", []string{"echoer"}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, "stopped", func() bool { s, _ := sv.Get("echoer"); return s.State.Status == "stopped" })

	if err := c.services("delete", []string{"echoer"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := sv.Get("echoer"); err != ErrServiceNotFound {
		t.Fatalf("after delete: %v", err)
	}
	if URLPort(p) != 8080 {
		t.Fatalf("URLPort with no http_port service = %d, want 8080", URLPort(p))
	}
}

func TestNeedsStartFirstAndCrashRestarts(t *testing.T) {
	p := testPaths(t)
	sv, c, _ := serve(t, p)

	if err := c.services("create", []string{"base", "--cmd", "/bin/sleep", "--args", "30", "--no-stream"}); err != nil {
		t.Fatal(err)
	}
	if err := c.services("create", []string{"top", "--cmd", "/bin/sleep", "--args", "30", "--needs", "base", "--no-stream"}); err != nil {
		t.Fatal(err)
	}
	if err := c.services("create", []string{"orphan", "--cmd", "/bin/true", "--needs", "missing", "--no-stream"}); err == nil {
		t.Fatal("a service needing an unknown one was accepted")
	}
	if err := sv.Stop("base", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := sv.Restart("top", time.Second); err != nil {
		t.Fatal(err)
	}
	if s, _ := sv.Get("base"); s.State.Status != "running" {
		t.Fatalf("starting top did not start base first: %+v", s.State)
	}

	if err := c.services("create", []string{"crasher", "--cmd", "/bin/sh", "--args", "-c,exit 7", "--no-stream"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a restart", func() bool { s, _ := sv.Get("crasher"); return s.State.RestartCount >= 1 })
}

func TestServicesStartAtAgentBoot(t *testing.T) {
	p := testPaths(t)
	_ = os.MkdirAll(p.ServicesDir(), 0o755)
	def, _ := json.Marshal(ServiceDef{Name: "boot", Cmd: "/bin/sleep", Args: []string{"30"}})
	if err := os.WriteFile(filepath.Join(p.ServicesDir(), "boot.json"), def, 0o644); err != nil {
		t.Fatal(err)
	}
	sv := NewSupervisor(p)
	defer sv.StopAll(time.Second)
	if s, err := sv.Get("boot"); err != nil || s.State.Status != "running" {
		t.Fatalf("defined service not started at boot: %+v %v", s.State, err)
	}
}

func TestTasksAndRefusedCheckpoints(t *testing.T) {
	p := testPaths(t)
	_, c, out := serve(t, p)
	cl := socketClient(c.sock)

	req, _ := http.NewRequest(http.MethodPut, "http://sprite/v1/tasks", strings.NewReader(`{"name":"agent-turn","expire":"2m"}`))
	resp, err := cl.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/tasks (the box's keep-awake call): %v %v", resp, err)
	}
	_ = resp.Body.Close()
	if err := c.request(http.MethodGet, "/v1/tasks", nil, nil, out); err != nil || !strings.Contains(out.String(), "agent-turn") {
		t.Fatalf("tasks: %v %s", err, out)
	}
	if err := c.request(http.MethodPost, "/v1/checkpoint", nil, map[string]string{}, out); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("checkpoint from inside: %v", err)
	}
}

func TestParseSignal(t *testing.T) {
	for in, ok := range map[string]bool{"TERM": true, "sigkill": true, "15": true, "nope": false} {
		if _, err := ParseSignal(in); (err == nil) != ok {
			t.Errorf("ParseSignal(%q): %v", in, err)
		}
	}
}
