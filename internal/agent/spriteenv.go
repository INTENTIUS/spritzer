//go:build !windows

package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const spriteEnvUsage = `sprite-env manages this sprite from the inside (spritzer container mode).

  sprite-env services create <name> --cmd <path> [--args a,b,c] [--env K=v,...] [--dir <path>]
                                    [--needs svc,...] [--http-port <port>] [--duration 5s] [--no-stream]
  sprite-env services list
  sprite-env services get <name>
  sprite-env services start <name>   [--duration 5s]
  sprite-env services restart <name> [--duration 5s]
  sprite-env services stop <name>    [--timeout 10s]
  sprite-env services signal <name> <signal>
  sprite-env services logs <name>    [--lines 100]
  sprite-env services delete <name>

  sprite-env curl [curl options] <path>   curl against the management socket, e.g. sprite-env curl /v1/services

Checkpoints and sprite creation from inside are not available in spritzer.
`

// SpriteEnv is the sprite-env CLI, compatible with wisp's for services. It
// returns the process exit code.
func SpriteEnv(p Paths, args []string) int {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, spriteEnvUsage)
		return 2
	}
	c := &envClient{sock: p.Socket(), out: os.Stdout}
	var err error
	switch args[0] {
	case "services", "service":
		err = c.services(args[1], args[2:])
	case "curl":
		err = c.curl(args[1:])
	case "checkpoints", "checkpoint", "sprites", "sprite":
		err = fmt.Errorf("%s are not available in spritzer's container exec mode", args[0])
	default:
		err = usageError(fmt.Sprintf("unknown command %q (run sprite-env for usage)", args[0]))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		if _, ok := err.(usageError); ok {
			return 2
		}
		return 1
	}
	return 0
}

type usageError string

func (e usageError) Error() string { return string(e) }

type envClient struct {
	sock string
	out  io.Writer
}

// parseArgs lets flags come before, between and after positionals.
func parseArgs(fs *flag.FlagSet, args []string, positionals int) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError(err.Error())
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != positionals {
		return nil, usageError(fmt.Sprintf("%s takes %d argument(s), got %d", fs.Name(), positionals, len(pos)))
	}
	return pos, nil
}

func splitList(v string) []string {
	if v == "" {
		return []string{}
	}
	return strings.Split(v, ",")
}

func (c *envClient) services(verb string, args []string) error {
	fs := flag.NewFlagSet("services "+verb, flag.ContinueOnError)
	switch verb {
	case "list", "ls":
		if _, err := parseArgs(fs, args, 0); err != nil {
			return err
		}
		return c.request(http.MethodGet, "/v1/services", nil, nil, c.out)
	case "get":
		pos, err := parseArgs(fs, args, 1)
		if err != nil {
			return err
		}
		return c.request(http.MethodGet, "/v1/services/"+url.PathEscape(pos[0]), nil, nil, c.out)
	case "logs":
		lines := fs.String("lines", "", "how many lines")
		pos, err := parseArgs(fs, args, 1)
		if err != nil {
			return err
		}
		q := url.Values{}
		if *lines != "" {
			q.Set("lines", *lines)
		}
		return c.request(http.MethodGet, "/v1/services/"+url.PathEscape(pos[0])+"/logs", q, nil, c.out)
	case "create":
		cmd := fs.String("cmd", "", "the executable to run")
		argv := fs.String("args", "", "comma-separated arguments")
		env := fs.String("env", "", "comma-separated KEY=value pairs")
		dir := fs.String("dir", "", "working directory")
		needs := fs.String("needs", "", "comma-separated services that start first")
		port := fs.Int("http-port", 0, "route the sprite URL to this port")
		duration := fs.String("duration", "5s", "how long to stream logs after starting")
		noStream := fs.Bool("no-stream", false, "print nothing")
		pos, err := parseArgs(fs, args, 1)
		if err != nil {
			return err
		}
		if *cmd == "" {
			return usageError("--cmd is required")
		}
		def := map[string]any{"cmd": *cmd, "args": splitList(*argv), "needs": splitList(*needs)}
		if *dir != "" {
			def["dir"] = *dir
		}
		if *port != 0 {
			def["http_port"] = *port
		}
		if *env != "" {
			vars := map[string]string{}
			for _, kv := range splitList(*env) {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return usageError(fmt.Sprintf("--env wants KEY=value pairs, got %q", kv))
				}
				vars[k] = v
			}
			def["env"] = vars
		}
		return c.stream(http.MethodPut, "/v1/services/"+url.PathEscape(pos[0]), *duration, *noStream, def)
	case "start", "restart":
		duration := fs.String("duration", "5s", "how long to stream logs after starting")
		noStream := fs.Bool("no-stream", false, "print nothing")
		pos, err := parseArgs(fs, args, 1)
		if err != nil {
			return err
		}
		return c.stream(http.MethodPost, "/v1/services/"+url.PathEscape(pos[0])+"/"+verb, *duration, *noStream, nil)
	case "stop":
		timeout := fs.String("timeout", "", "how long before SIGKILL")
		pos, err := parseArgs(fs, args, 1)
		if err != nil {
			return err
		}
		q := url.Values{}
		if *timeout != "" {
			q.Set("timeout", *timeout)
		}
		return c.request(http.MethodPost, "/v1/services/"+url.PathEscape(pos[0])+"/stop", q, nil, c.out)
	case "signal":
		pos, err := parseArgs(fs, args, 2)
		if err != nil {
			return err
		}
		return c.request(http.MethodPost, "/v1/services/signal", nil, map[string]string{"name": pos[0], "signal": pos[1]}, c.out)
	case "delete", "rm":
		pos, err := parseArgs(fs, args, 1)
		if err != nil {
			return err
		}
		return c.request(http.MethodDelete, "/v1/services/"+url.PathEscape(pos[0]), nil, nil, c.out)
	}
	return usageError(fmt.Sprintf("unknown services command %q", verb))
}

func (c *envClient) stream(method, path, duration string, quiet bool, body any) error {
	q := url.Values{"duration": {duration}}
	if quiet {
		q.Set("duration", "0s")
		return c.request(method, path, q, body, io.Discard)
	}
	return c.request(method, path, q, body, c.out)
}

func (c *envClient) request(method, path string, q url.Values, body any, out io.Writer) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	u := "http://sprite" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := socketClient(c.sock).Do(req)
	if err != nil {
		return fmt.Errorf("management socket %s: %v", c.sock, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return fmt.Errorf("%s (%d)", e.Message, resp.StatusCode)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, bytes.TrimSpace(b), "", "  ") == nil {
			b = append(pretty.Bytes(), '\n')
		}
		_, err = out.Write(b)
		return err
	}
	var failed string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		_, _ = fmt.Fprintln(out, sc.Text())
		var ev struct{ Type, Data string }
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "error" {
			failed = ev.Data
		}
	}
	if failed != "" {
		return fmt.Errorf("%s", failed)
	}
	return sc.Err()
}

func (c *envClient) curl(args []string) error {
	bin, err := exec.LookPath("curl")
	if err != nil {
		return err
	}
	argv := []string{"curl", "--unix-socket", c.sock}
	for _, a := range args {
		if strings.HasPrefix(a, "/v1/") {
			a = "http://sprite" + a
		}
		argv = append(argv, a)
	}
	return syscall.Exec(bin, argv, os.Environ())
}
