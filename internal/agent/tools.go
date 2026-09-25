//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Install copies the running binary to dir/spritzer and links dir/sprite-env
// to it. It is the init step that puts the agent into a sprite: the runtime
// runs `spritzer agent-install /.sprite/bin` from spritzer's own image into a
// volume the sprite container then mounts.
func Install(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp := filepath.Join(dir, ".spritzer.tmp")
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "spritzer")); err != nil {
		return err
	}
	link := filepath.Join(dir, "sprite-env")
	_ = os.Remove(link)
	return os.Symlink("spritzer", link)
}

// ExecWrap is `spritzer x-exec`: it becomes the user's command, after setting
// the working directory and environment the exec asked for, in a process group
// of its own whose leader's pid is recorded under the exec id. When a client
// disconnects before the command exits, the host runs `x-kill <id>`, since
// neither Docker nor Kubernetes kills an exec'd process when its stream closes.
func ExecWrap(p Paths, args []string) int {
	fs := flag.NewFlagSet("x-exec", flag.ContinueOnError)
	id := fs.String("id", "", "exec id for x-kill")
	dir := fs.String("dir", "", "working directory")
	var envs multiFlag
	fs.Var(&envs, "env", "KEY=value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "x-exec: no command")
		return ExitUsage
	}
	_ = syscall.Setpgid(0, 0)
	if *id != "" && validID(*id) {
		_ = os.MkdirAll(p.ExecDir(), 0o755)
		_ = os.WriteFile(filepath.Join(p.ExecDir(), *id), []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	env := serviceEnv(p)
	for _, kv := range envs {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		env = setEnv(env, kv)
	}
	if *dir != "" {
		if err := os.Chdir(*dir); err != nil {
			fmt.Fprintf(os.Stderr, "x-exec: %v\n", err)
			return 126
		}
	} else if h := lookupEnv(env, "HOME"); h != "" {
		_ = os.Chdir(h)
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		_ = os.Setenv(k, v)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: command not found\n", argv[0])
		return 127
	}
	err = syscall.Exec(path, argv, env)
	fmt.Fprintf(os.Stderr, "x-exec: %v\n", err)
	return 126
}

// Kill is `spritzer x-kill <id>`: SIGTERM, then SIGKILL, to an exec's group.
func Kill(p Paths, id string) int {
	if !validID(id) {
		return ExitUsage
	}
	f := filepath.Join(p.ExecDir(), id)
	b, err := os.ReadFile(f)
	if err != nil {
		return ExitNotFound
	}
	_ = os.Remove(f)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return ExitNotFound
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		if syscall.Kill(pid, 0) != nil {
			return ExitOK
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return ExitOK
}

// Forget is `spritzer x-forget <id>`: drop an exec's pid file after it exited.
func Forget(p Paths, id string) int {
	if validID(id) {
		_ = os.Remove(filepath.Join(p.ExecDir(), id))
	}
	return ExitOK
}

// Relay is `spritzer x-relay <target>`: it joins its stdin and stdout to a
// connection. The host dials the agent's socket and the sprite's HTTP port this
// way, over an exec, so nothing inside the sprite needs to be reachable.
//
// target is unix:<path>, tcp:<host:port>, or "url", which is the port the
// sprite URL serves: the one service with an http_port, else 8080. A port with
// nothing listening yet is retried for a few seconds, as a service that has
// just been started may not have bound it.
func Relay(p Paths, target string) int {
	network, addr := "", ""
	switch {
	case strings.HasPrefix(target, "unix:"):
		network, addr = "unix", strings.TrimPrefix(target, "unix:")
	case strings.HasPrefix(target, "tcp:"):
		network, addr = "tcp", strings.TrimPrefix(target, "tcp:")
	case target == "url":
		network, addr = "tcp", "127.0.0.1:"+strconv.Itoa(URLPort(p))
	default:
		fmt.Fprintf(os.Stderr, "x-relay: bad target %q\n", target)
		return ExitUsage
	}
	var conn net.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err = net.DialTimeout(network, addr, 2*time.Second)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "x-relay: %v\n", err)
		return ExitFailed
	}
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	// The far end closing is the end of the relay. stdin reaching EOF only
	// half-closes, so a response still in flight is delivered.
	_, _ = io.Copy(os.Stdout, conn)
	_ = conn.Close()
	return ExitOK
}

// URLPort is the port the sprite URL routes to: the service with an http_port,
// else 8080, as on Sprites and wisp.
func URLPort(p Paths) int {
	entries, _ := os.ReadDir(p.ServicesDir())
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(p.ServicesDir(), e.Name()))
		if err != nil {
			continue
		}
		var def ServiceDef
		if json.Unmarshal(b, &def) == nil && def.HTTPPort != nil && *def.HTTPPort > 0 {
			return *def.HTTPPort
		}
	}
	return 8080
}

// DirEntry matches the interpreter's fs/list entries.
type DirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

// FS is `spritzer x-fs <read|write|list|delete> <path> [--recursive]`, backing
// the filesystem API in container mode. Missing paths exit ExitNotFound.
func FS(args []string) int {
	if len(args) < 2 {
		return ExitUsage
	}
	op, path := args[0], args[1]
	switch op {
	case "read":
		f, err := os.Open(path)
		if err != nil {
			return fsErr(err)
		}
		defer func() { _ = f.Close() }()
		if st, err := f.Stat(); err == nil && st.IsDir() {
			fmt.Fprintln(os.Stderr, "is a directory")
			return ExitFailed
		}
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return ExitFailed
		}
		return ExitOK
	case "write":
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fsErr(err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".spritzer-write-*")
		if err != nil {
			return fsErr(err)
		}
		if _, err := io.Copy(tmp, os.Stdin); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return ExitFailed
		}
		_ = tmp.Chmod(0o644)
		_ = tmp.Close()
		if err := os.Rename(tmp.Name(), path); err != nil {
			_ = os.Remove(tmp.Name())
			return fsErr(err)
		}
		return ExitOK
	case "list":
		entries, err := os.ReadDir(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// The interpreter lists a missing directory as empty.
				fmt.Println("[]")
				return ExitOK
			}
			return fsErr(err)
		}
		var dirs, files []DirEntry
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, DirEntry{Name: e.Name(), Type: "dir"})
				continue
			}
			var size int64
			if info, err := e.Info(); err == nil {
				size = info.Size()
			}
			files = append(files, DirEntry{Name: e.Name(), Type: "file", Size: size})
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		out := append(append([]DirEntry{}, dirs...), files...)
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return ExitOK
	case "delete":
		recursive := len(args) > 2 && args[2] == "--recursive"
		if _, err := os.Lstat(path); err != nil {
			return fsErr(err)
		}
		var err error
		if recursive {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
		if err != nil {
			return fsErr(err)
		}
		return ExitOK
	}
	return ExitUsage
}

func fsErr(err error) int {
	fmt.Fprintln(os.Stderr, err)
	if errors.Is(err, os.ErrNotExist) {
		return ExitNotFound
	}
	return ExitFailed
}

// Ping exits 0 once the agent answers on its socket.
func Ping(p Paths) int {
	c := socketClient(p.Socket())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://sprite/_agent/health", nil)
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitFailed
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ExitFailed
	}
	return ExitOK
}

func socketClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func setEnv(env []string, kv string) []string {
	k, _, _ := strings.Cut(kv, "=")
	for i, e := range env {
		if strings.HasPrefix(e, k+"=") {
			env[i] = kv
			return env
		}
	}
	return append(env, kv)
}

func lookupEnv(env []string, k string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, k+"="); ok {
			return v
		}
	}
	return ""
}
