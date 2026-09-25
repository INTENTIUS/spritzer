package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Docker runs each sprite as a container named spritzer-<name>, talking to the
// Engine API on a unix socket (DOCKER_HOST=unix://..., else
// /var/run/docker.sock). The agent binary reaches sprites through a named
// volume, filled once at startup by running spritzer's own image with
// `agent-install`, and mounted read-only at /.sprite/bin. Containers run with
// Docker's init, so the agent is not PID 1 and orphans are reaped.
type Docker struct {
	cfg    Config
	socket string
	hc     *http.Client
	volume string

	prepOnce sync.Once
	prepErr  error
}

// NewDocker connects to the Docker socket.
func NewDocker(cfg Config) (*Docker, error) {
	sock := "/var/run/docker.sock"
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		if !strings.HasPrefix(h, "unix://") {
			return nil, fmt.Errorf("DOCKER_HOST %q: only unix:// sockets are supported", h)
		}
		sock = strings.TrimPrefix(h, "unix://")
	}
	d := &Docker{cfg: cfg, socket: sock}
	d.hc = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "unix", sock)
		},
	}}
	sum := sha256.Sum256([]byte(cfg.AgentImage))
	d.volume = "spritzer-agent-" + hex.EncodeToString(sum[:6])
	return d, nil
}

// Kind implements Backend.
func (d *Docker) Kind() string { return "docker" }

func containerName(name string) string { return "spritzer-" + name }

type dockerErr struct {
	Message string `json:"message"`
}

// call does one Engine API request. out may be nil. It returns the status.
func (d *Docker) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var e dockerErr
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return resp.StatusCode, fmt.Errorf("docker %s %s: %s", method, path, e.Message)
		}
		return resp.StatusCode, fmt.Errorf("docker %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// ensureImage pulls ref unless it is already present.
func (d *Docker) ensureImage(ctx context.Context, ref string) error {
	if st, _ := d.call(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil); st == http.StatusOK {
		return nil
	}
	image, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		image, tag = ref[:i], ref[i+1:]
	}
	q := url.Values{"fromImage": {image}, "tag": {tag}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/images/create?"+q.Encode(), nil)
	resp, err := d.hc.Do(req)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The pull reports progress, and failure, as a JSON stream.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("pull %s: %w", ref, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, msg.Error)
		}
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("pull %s: %s", ref, resp.Status)
	}
	return nil
}

// prepare fills the agent volume from the agent image, once per process.
func (d *Docker) prepare(ctx context.Context) error {
	d.prepOnce.Do(func() { d.prepErr = d.fillAgentVolume(ctx) })
	return d.prepErr
}

func (d *Docker) fillAgentVolume(ctx context.Context) error {
	if err := d.ensureImage(ctx, d.cfg.AgentImage); err != nil {
		return err
	}
	if _, err := d.call(ctx, http.MethodPost, "/volumes/create", map[string]any{
		"Name":   d.volume,
		"Labels": map[string]string{LabelManagedBy: ManagedBy},
	}, nil); err != nil {
		return err
	}
	var created struct{ ID string }
	if _, err := d.call(ctx, http.MethodPost, "/containers/create", map[string]any{
		"Image":  d.cfg.AgentImage,
		"Cmd":    []string{"agent-install", "/out"},
		"User":   "0",
		"Labels": map[string]string{LabelManagedBy: ManagedBy},
		"HostConfig": map[string]any{
			"Mounts": []map[string]any{{"Type": "volume", "Source": d.volume, "Target": "/out"}},
		},
	}, &created); err != nil {
		return err
	}
	defer func() {
		_, _ = d.call(context.Background(), http.MethodDelete, "/containers/"+created.ID+"?force=1", nil, nil)
	}()
	if _, err := d.call(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return err
	}
	var waited struct{ StatusCode int }
	if _, err := d.call(ctx, http.MethodPost, "/containers/"+created.ID+"/wait", nil, &waited); err != nil {
		return err
	}
	if waited.StatusCode != 0 {
		return fmt.Errorf("agent-install from %s exited %d", d.cfg.AgentImage, waited.StatusCode)
	}
	return nil
}

// Create implements Backend.
func (d *Docker) Create(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.CreateTimeout)
	defer cancel()
	if err := d.prepare(ctx); err != nil {
		return err
	}
	cn := containerName(name)
	var inspect struct {
		State struct{ Running bool }
	}
	st, _ := d.call(ctx, http.MethodGet, "/containers/"+cn+"/json", nil, &inspect)
	if st == http.StatusNotFound {
		if err := d.ensureImage(ctx, d.cfg.SpriteImage); err != nil {
			return err
		}
		body := map[string]any{
			"Image":      d.cfg.SpriteImage,
			"Entrypoint": []string{AgentBinary},
			"Cmd":        []string{"agent"},
			"Hostname":   name,
			"Labels":     map[string]string{LabelManagedBy: ManagedBy, LabelSprite: name},
			"HostConfig": map[string]any{
				"Init":          true,
				"RestartPolicy": map[string]any{"Name": "unless-stopped"},
				"Mounts": []map[string]any{{
					"Type": "volume", "Source": d.volume, "Target": BinDir, "ReadOnly": true,
				}},
			},
		}
		if _, err := d.call(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(cn), body, nil); err != nil {
			return err
		}
	}
	if !inspect.State.Running {
		if _, err := d.call(ctx, http.MethodPost, "/containers/"+cn+"/start", nil, nil); err != nil {
			return err
		}
	}
	return waitForAgent(ctx, d, name)
}

// Delete implements Backend.
func (d *Docker) Delete(ctx context.Context, name string) error {
	st, err := d.call(ctx, http.MethodDelete, "/containers/"+containerName(name)+"?force=1&v=1", nil, nil)
	if st == http.StatusNotFound {
		return ErrNotFound
	}
	return err
}

// List implements Backend.
func (d *Docker) List(ctx context.Context) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {LabelManagedBy + "=" + ManagedBy, LabelSprite}})
	var cs []struct {
		Labels map[string]string
	}
	if _, err := d.call(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &cs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if n := c.Labels[LabelSprite]; n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// Exec implements Backend: create an exec, then start it on a hijacked
// connection that carries Docker's multiplexed stdout/stderr frames.
func (d *Docker) Exec(ctx context.Context, name string, argv []string, stdin bool) (Process, error) {
	var created struct{ ID string }
	st, err := d.call(ctx, http.MethodPost, "/containers/"+containerName(name)+"/exec", map[string]any{
		"AttachStdin": stdin, "AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": argv,
	}, &created)
	if st == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "unix", d.socket)
	if err != nil {
		return nil, err
	}
	body := `{"Detach":false,"Tty":false}`
	if _, err := fmt.Fprintf(conn, "POST /exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\n"+
		"Connection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n%s", created.ID, len(body), body); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("docker exec start: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("docker exec start: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = conn.Close()
		return nil, fmt.Errorf("docker exec start: %s %s", resp.Status, bytes.TrimSpace(b))
	}
	p := &dockerProc{d: d, id: created.ID, conn: conn, done: make(chan struct{})}
	p.outR, p.outW = io.Pipe()
	p.errR, p.errW = io.Pipe()
	go p.demux(br)
	return p, nil
}

type dockerProc struct {
	d          *Docker
	id         string
	conn       net.Conn
	outR, errR *io.PipeReader
	outW, errW *io.PipeWriter
	done       chan struct{}
	closeOnce  sync.Once
}

// demux splits Docker's 8-byte-header frames into stdout and stderr.
func (p *dockerProc) demux(r io.Reader) {
	defer close(p.done)
	hdr := make([]byte, 8)
	var err error
	for {
		if _, err = io.ReadFull(r, hdr); err != nil {
			break
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		var w io.Writer = p.outW
		if hdr[0] == 2 {
			w = p.errW
		}
		if _, err = io.CopyN(w, r, n); err != nil {
			break
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	_ = p.outW.CloseWithError(err)
	_ = p.errW.CloseWithError(err)
}

func (p *dockerProc) Stdin() io.WriteCloser { return dockerStdin{p} }
func (p *dockerProc) Stdout() io.Reader     { return p.outR }
func (p *dockerProc) Stderr() io.Reader     { return p.errR }

type dockerStdin struct{ p *dockerProc }

func (s dockerStdin) Write(b []byte) (int, error) { return s.p.conn.Write(b) }
func (s dockerStdin) Close() error {
	if cw, ok := s.p.conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Wait implements Process.
func (p *dockerProc) Wait() (int, error) {
	<-p.done
	for i := 0; i < 50; i++ {
		var info struct {
			Running  bool
			ExitCode int
		}
		if _, err := p.d.call(context.Background(), http.MethodGet, "/exec/"+p.id+"/json", nil, &info); err != nil {
			return -1, err
		}
		if !info.Running {
			return info.ExitCode, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return -1, errors.New("docker exec: stream ended but the process is still running")
}

// Close implements Process.
func (p *dockerProc) Close() error {
	p.closeOnce.Do(func() { _ = p.conn.Close() })
	return nil
}
