//go:build e2e

// Package e2e drives a spritzer running in container exec mode against a real
// runtime. It is the acceptance for INTENTIUS/spritzer#22. Run it through
// scripts/e2e-docker.sh or scripts/e2e-k3d.sh (just e2e-docker / just
// e2e-real), which start spritzer and set:
//
//	SPRITZER_E2E_URL        spritzer's base URL
//	SPRITZER_E2E_RUNTIME    docker | kubernetes, to check the container is gone
//	SPRITZER_E2E_NAMESPACE  the sprites' namespace (kubernetes)
//	SPRITZER_E2E_CONTEXT    the kubectl context (kubernetes)
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type execResult struct {
	stdout, stderr string
	code           int
}

func base(t *testing.T) string {
	u := os.Getenv("SPRITZER_E2E_URL")
	if u == "" {
		t.Skip("SPRITZER_E2E_URL is not set")
	}
	return strings.TrimRight(u, "/")
}

// run execs over the control WebSocket: stdin (if any) then StreamStdinEOF,
// collecting stdout, stderr and the exit frame.
func run(t *testing.T, sprite string, q url.Values, stdin string) execResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	u := strings.Replace(base(t), "http", "ws", 1) + "/v1/sprites/" + sprite + "/exec?" + q.Encode()
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("exec dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(64 << 20)
	if stdin != "" {
		if err := c.Write(ctx, websocket.MessageBinary, append([]byte{0}, stdin...)); err != nil {
			t.Fatalf("stdin: %v", err)
		}
	}
	if q.Get("stdin") != "false" {
		if err := c.Write(ctx, websocket.MessageBinary, []byte{4}); err != nil {
			t.Fatalf("stdin eof: %v", err)
		}
	}
	var out, errb bytes.Buffer
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("exec read before exit frame: %v (stdout %q stderr %q)", err, out.String(), errb.String())
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case 1:
			out.Write(data[1:])
		case 2:
			errb.Write(data[1:])
		case 3:
			return execResult{stdout: out.String(), stderr: errb.String(), code: int(data[1])}
		}
	}
}

func argv(args ...string) url.Values {
	q := url.Values{}
	for _, a := range args {
		q.Add("cmd", a)
	}
	return q
}

func call(t *testing.T, method, path string, body io.Reader, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, base(t)+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	c := &http.Client{Timeout: 6 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestContainerModeAcceptance(t *testing.T) {
	b := base(t)
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := "e2e-" + hex.EncodeToString(suffix)

	var health struct {
		Exec    string `json:"exec"`
		Runtime string `json:"runtime"`
	}
	_, hb := call(t, http.MethodGet, "/_spritzer/health", nil, "")
	_ = json.Unmarshal([]byte(hb), &health)
	if health.Exec != "container" {
		t.Fatalf("spritzer at %s is not in container exec mode: %s", b, hb)
	}
	t.Logf("spritzer %s: exec=%s runtime=%s", b, health.Exec, health.Runtime)

	// 1. create
	start := time.Now()
	st, body := call(t, http.MethodPost, "/v1/sprites", strings.NewReader(`{"name":"`+name+`"}`), "")
	if st != http.StatusCreated {
		t.Fatalf("create: %d %s", st, body)
	}
	var created struct{ ID, URL string }
	_ = json.Unmarshal([]byte(body), &created)
	t.Logf("created sprite %s in %s, url %s", name, time.Since(start).Round(time.Millisecond), created.URL)
	destroyed := false
	defer func() {
		if !destroyed {
			call(t, http.MethodDelete, "/v1/sprites/"+name, nil, "")
		}
	}()

	// 2. uname -a, as an argv
	r := run(t, name, argv("uname", "-a"), "")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "Linux ") || !strings.Contains(r.stdout, name) {
		t.Fatalf("uname -a: code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	t.Logf("uname -a -> %s", strings.TrimSpace(r.stdout))

	// 3. a multi-line script, with stderr, a non-zero exit, env and dir
	script := `set -e
x=$(( 6 * 7 ))
echo "answer $x"
echo "greeting $GREETING"
pwd
echo "to stderr" >&2
for i in 1 2 3; do printf '%s,' "$i"; done; echo
exit 3`
	q := argv("bash", "-c", script)
	q.Add("env", "GREETING=hello from env")
	q.Set("dir", "/tmp")
	r = run(t, name, q, "")
	want := "answer 42\ngreeting hello from env\n/tmp\n1,2,3,\n"
	if r.code != 3 || r.stdout != want || r.stderr != "to stderr\n" {
		t.Fatalf("script: code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	t.Logf("multi-line script -> exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)

	// A single cmd param holding a command line runs under sh -c, and stdin
	// reaches the process.
	r = run(t, name, url.Values{"cmd": {"tr a-z A-Z | sed 's/^/got: /'"}}, "real stdin\n")
	if r.code != 0 || r.stdout != "got: REAL STDIN\n" {
		t.Fatalf("stdin pipe: code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	t.Logf("stdin through a pipeline -> %q", r.stdout)

	// An exec whose client goes away is killed in the sprite.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		q := argv("bash", "-c", "echo $$ > /tmp/orphan.pid; echo started; exec sleep 300")
		u := strings.Replace(b, "http", "ws", 1) + "/v1/sprites/" + name + "/exec?" + q.Encode()
		c, _, err := websocket.Dial(ctx, u, nil)
		if err != nil {
			t.Fatalf("exec dial: %v", err)
		}
		if _, data, err := c.Read(ctx); err != nil || string(data[1:]) != "started\n" {
			t.Fatalf("orphan exec: %q %v", data, err)
		}
		_ = c.CloseNow()
		check := argv("bash", "-c", `kill -0 "$(cat /tmp/orphan.pid)" 2>/dev/null && echo alive || echo gone`)
		for i := 0; ; i++ {
			if r := run(t, name, check, ""); r.stdout == "gone\n" {
				break
			} else if i == 20 {
				t.Fatalf("the process of a disconnected exec is still running: %q", r.stdout)
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Logf("disconnected exec -> its process was killed")
	}()

	// 4. a service, started from inside with sprite-env, serving on 8080
	st, _ = call(t, http.MethodPut, "/v1/sprites/"+name+"/fs/write?path=/srv/www/hello.txt", strings.NewReader("hello from the sprite\n"), "")
	if st != http.StatusOK {
		t.Fatalf("fs write: %d", st)
	}
	r = run(t, name, argv("sprite-env", "services", "create", "web",
		"--cmd", "python3", "--args", "-m,http.server,8080", "--dir", "/srv/www",
		"--http-port", "8080", "--duration", "1s"), "")
	if r.code != 0 || !strings.Contains(r.stdout, `"type":"started"`) {
		t.Fatalf("sprite-env services create: code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	t.Logf("sprite-env services create web -> exit 0 (the exec that started it has ended)")

	fetch := func(label string) {
		t.Helper()
		var st int
		var body string
		for i := 0; i < 20; i++ {
			st, body = call(t, http.MethodGet, "/s/"+name+"/hello.txt", nil, "")
			if st == http.StatusOK {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if st != http.StatusOK || body != "hello from the sprite\n" {
			t.Fatalf("%s: GET /s/%s/hello.txt: %d %q", label, name, st, body)
		}
		t.Logf("%s: GET /s/%s/hello.txt -> %d %q", label, name, st, body)
	}
	fetch("sprite URL")
	time.Sleep(3 * time.Second)
	fetch("sprite URL, 3s after the exec ended")

	if u, err := url.Parse(created.URL); err == nil && !strings.HasPrefix(u.Path, "/s/") {
		st, body = call(t, http.MethodGet, "/hello.txt", nil, u.Host)
		if st != http.StatusOK || body != "hello from the sprite\n" {
			t.Fatalf("host-routed URL %s: %d %q", created.URL, st, body)
		}
		t.Logf("host-routed URL %s/hello.txt -> %d %q", created.URL, st, body)
	}

	st, body = call(t, http.MethodGet, "/v1/sprites/"+name+"/services/web", nil, "")
	if st != http.StatusOK || !strings.Contains(body, `"status":"running"`) {
		t.Fatalf("GET services/web: %d %s", st, body)
	}
	t.Logf("GET /v1/sprites/%s/services/web -> running", name)

	// 5. destroy, and the container or pod is gone
	st, body = call(t, http.MethodDelete, "/v1/sprites/"+name, nil, "")
	if st != http.StatusOK {
		t.Fatalf("destroy: %d %s", st, body)
	}
	destroyed = true
	if st, _ = call(t, http.MethodGet, "/v1/sprites/"+name, nil, ""); st != http.StatusNotFound {
		t.Fatalf("GET after destroy: %d", st)
	}
	assertGone(t, name)
}

func assertGone(t *testing.T, name string) {
	t.Helper()
	switch os.Getenv("SPRITZER_E2E_RUNTIME") {
	case "docker":
		out, err := exec.Command("docker", "ps", "-a", "--filter", "name=^spritzer-"+name+"$", "--format", "{{.Names}}").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Fatalf("container spritzer-%s still present: %q %v", name, out, err)
		}
		t.Logf("destroyed: docker has no container spritzer-%s", name)
	case "kubernetes":
		args := []string{"get", "pod", "sprite-" + name, "-n", os.Getenv("SPRITZER_E2E_NAMESPACE")}
		if c := os.Getenv("SPRITZER_E2E_CONTEXT"); c != "" {
			args = append(args, "--context", c)
		}
		out, err := exec.Command("kubectl", args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "NotFound") {
			t.Fatalf("pod sprite-%s still present: %s", name, out)
		}
		t.Logf("destroyed: %s", strings.TrimSpace(string(out)))
	default:
		t.Logf("SPRITZER_E2E_RUNTIME unset: not checking the runtime directly")
	}
}
