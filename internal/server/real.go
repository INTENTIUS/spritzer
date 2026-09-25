package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/intentius/spritzer/internal/runtime"
	"github.com/intentius/spritzer/internal/sprite"
)

// This file is container exec mode (SPRITZER_EXEC=container). Each sprite is
// a container from runtime.Backend; exec runs real commands in it; services,
// the filesystem API and the sprite URL reach the container through the
// in-sprite agent (internal/agent) over execs. The interpreter handlers in
// server.go, ws.go and config.go are untouched and serve when Runtime is nil.
//
// The store still holds each sprite's record (status, URL, network policy,
// tasks), so GET /v1/sprites/{id} and the policy and task endpoints behave as
// they do in interpreter mode. Its fs map is unused here.

// realPaths are added to the health/coverage list in container mode.
var realPaths = []string{
	"GET /v1/sprites (container mode)",
	"/v1/sprites/{id}/services/... (container mode: proxied to the in-sprite agent)",
	"/s/{name}/... and {name}.<SPRITZER_URL_DOMAIN> (container mode: the sprite URL)",
}

type realMode struct {
	rt        runtime.Backend
	urlDomain string

	mu         sync.Mutex
	transports map[string]*http.Transport
}

func newRealMode(rt runtime.Backend, urlDomain string) *realMode {
	return &realMode{rt: rt, urlDomain: strings.TrimPrefix(urlDomain, "."), transports: map[string]*http.Transport{}}
}

// transport is a pooled HTTP transport whose connections are x-relay execs
// into the sprite, to target (unix:<socket> or "url").
func (m *realMode) transport(name, target string) *http.Transport {
	key := name + "\x00" + target
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.transports[key]; ok {
		return t
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return runtime.Dial(ctx, m.rt, name, target)
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	m.transports[key] = t
	return t
}

// forget drops the pooled connections to a destroyed sprite.
func (m *realMode) forget(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, t := range m.transports {
		if strings.HasPrefix(key, name+"\x00") {
			t.CloseIdleConnections()
			delete(m.transports, key)
		}
	}
}

// Adopt records every sprite container the runtime already holds, so a
// restarted spritzer (a new pod, say) still knows the sprites it made.
func (s *Server) Adopt(ctx context.Context) error {
	if s.real == nil {
		return nil
	}
	names, err := s.real.rt.List(ctx)
	if err != nil {
		return err
	}
	for _, n := range names {
		if _, err := s.store.Get(n); err != nil {
			s.store.Create(n, "", nil)
		}
	}
	return nil
}

// realURL is the sprite's URL: <name>.<domain> when a URL domain is set,
// else the path form /s/<name> on spritzer's own host.
func (s *Server) realURL(host, name string) string {
	if s.real.urlDomain != "" {
		port := ""
		if _, p, err := net.SplitHostPort(host); err == nil && p != "80" {
			port = ":" + p
		}
		return "http://" + name + "." + s.real.urlDomain + port
	}
	return spriteURL(host, name)
}

func (s *Server) realRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sprites", s.realCreate)
	mux.HandleFunc("GET /v1/sprites", s.realList)
	mux.HandleFunc("GET /v1/sprites/{id}", s.realGet)
	mux.HandleFunc("DELETE /v1/sprites/{id}", s.realDestroy)
	mux.HandleFunc("GET /v1/sprites/{id}/exec", s.realExec)

	refuse := func(w http.ResponseWriter, _ *http.Request) {
		s.writeError(w, http.StatusNotImplemented,
			"checkpoints are not available in container exec mode (SPRITZER_EXEC=container); see INTENTIUS/spritzer#23")
	}
	mux.HandleFunc("POST /v1/sprites/{id}/checkpoint", refuse)
	mux.HandleFunc("GET /v1/sprites/{id}/checkpoints", refuse)
	mux.HandleFunc("GET /v1/sprites/{id}/checkpoints/{cid}", refuse)
	mux.HandleFunc("POST /v1/sprites/{id}/checkpoints/{cid}/restore", refuse)

	mux.HandleFunc("PUT /v1/sprites/{id}/fs/write", s.realFSWrite)
	mux.HandleFunc("GET /v1/sprites/{id}/fs/read", s.realFSRead)
	mux.HandleFunc("GET /v1/sprites/{id}/fs/list", s.realFSList)
	mux.HandleFunc("DELETE /v1/sprites/{id}/fs/delete", s.realFSDelete)

	mux.HandleFunc("GET /v1/sprites/{id}/policy/network", s.getPolicy)
	mux.HandleFunc("POST /v1/sprites/{id}/policy/network", s.setPolicy)

	mux.HandleFunc("/v1/sprites/{id}/services", s.realServices)
	mux.HandleFunc("/v1/sprites/{id}/services/{rest...}", s.realServices)

	mux.HandleFunc("GET /v1/sprites/{id}/tasks", s.listTasks)
	mux.HandleFunc("POST /v1/sprites/{id}/tasks", s.createTask)
	mux.HandleFunc("PUT /v1/sprites/{id}/tasks/{name}", s.refreshTask)
	mux.HandleFunc("DELETE /v1/sprites/{id}/tasks/{name}", s.releaseTask)

	mux.HandleFunc("/s/{name}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/s/"+url.PathEscape(r.PathValue("name"))+"/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/s/{name}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		s.proxySpriteURL(w, r, name, "/"+r.PathValue("rest"), "/s/"+name)
	})
}

// hostRouted serves <name>.<urlDomain> as that sprite's URL, and everything
// else through the API mux.
func (s *Server) hostRouted(next http.Handler) http.Handler {
	suffix := "." + s.real.urlDomain
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if name, ok := strings.CutSuffix(strings.ToLower(host), suffix); ok && name != "" && !strings.Contains(name, ".") {
			s.proxySpriteURL(w, r, name, r.URL.Path, "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) realCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := runtime.ValidName(req.Name); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A create runs to the end even if the caller goes away: an abandoned
	// half-made container is worse than a finished one nobody asked about.
	ctx := context.WithoutCancel(r.Context())
	if err := s.real.rt.Create(ctx, req.Name); err != nil {
		s.log.Error("sprite create failed", "name", req.Name, "err", err)
		s.writeError(w, http.StatusBadGateway, "create sprite "+req.Name+": "+err.Error())
		return
	}
	u := s.realURL(r.Host, req.Name)
	if _, err := s.store.Get(req.Name); err != nil {
		s.store.Create(req.Name, u, req.Policy)
	}
	writeJSON(w, http.StatusCreated, createResponse{ID: req.Name, URL: u})
}

type listedSprite struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Status sprite.Status `json:"status"`
	URL    string        `json:"url"`
}

func (s *Server) realList(w http.ResponseWriter, r *http.Request) {
	names, err := s.real.rt.List(r.Context())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]listedSprite, 0, len(names))
	for _, n := range names {
		if _, err := s.store.Get(n); err != nil {
			s.store.Create(n, "", nil)
		}
		out = append(out, listedSprite{ID: n, Name: n, Status: sprite.StatusRunning, URL: s.realURL(r.Host, n)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sprites": out, "has_more": false})
}

func (s *Server) realGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	view, err := s.store.Get(id)
	if s.handleLookupError(w, id, err) {
		return
	}
	if view.URL == "" {
		view.URL = s.realURL(r.Host, id)
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) realDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, lookupErr := s.store.Get(id)
	err := s.real.rt.Delete(context.WithoutCancel(r.Context()), id)
	s.real.forget(id)
	if errors.Is(err, runtime.ErrNotFound) {
		if lookupErr != nil {
			s.writeError(w, http.StatusNotFound, "no sprite "+id)
			return
		}
		err = nil
	}
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "destroy sprite "+id+": "+err.Error())
		return
	}
	_ = s.store.Destroy(id)
	writeJSON(w, http.StatusOK, struct{}{})
}

// known reports whether id is a live sprite, writing the 404 if not.
func (s *Server) known(w http.ResponseWriter, id string) bool {
	_, err := s.store.Get(id)
	return !s.handleLookupError(w, id, err)
}

// execArgv builds the command from the exec query. Repeated cmd params are an
// argv, as the Sprites SDKs send it. A single cmd that looks like a command
// line (spaces, quotes, shell operators) runs under /bin/sh -c, which is how
// the interpreter-era callers send one; path alone is argv[0].
func execArgv(q url.Values) []string {
	cmds := q["cmd"]
	if len(cmds) == 0 {
		if p := q.Get("path"); p != "" {
			return []string{p}
		}
		return nil
	}
	if len(cmds) == 1 && strings.ContainsAny(cmds[0], " \t\n;|&$<>()'\"`*?~") {
		return []string{"/bin/sh", "-c", cmds[0]}
	}
	return cmds
}

func newExecID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// realExec runs the command in the sprite and carries it over the same framed
// control WebSocket the interpreter uses: stdin frames in, stdout and stderr
// frames out as they are produced, then the exit frame. If the client goes
// away first, the process is killed, as Sprites does without
// max_run_after_disconnect.
func (s *Server) realExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isWebSocketUpgrade(r) {
		s.listExecSessions(w, r)
		return
	}
	if !s.known(w, id) {
		return
	}
	q := r.URL.Query()
	argv := execArgv(q)
	if len(argv) == 0 {
		s.writeError(w, http.StatusBadRequest, "cmd or path is required")
		return
	}
	execID := newExecID()
	wrapped := []string{runtime.AgentBinary, "x-exec", "--id", execID}
	if d := q.Get("dir"); d != "" {
		wrapped = append(wrapped, "--dir", d)
	}
	for _, kv := range q["env"] {
		wrapped = append(wrapped, "--env", kv)
	}
	wrapped = append(append(wrapped, "--"), argv...)
	wantStdin := q.Get("stdin") != "false"

	proc, err := s.real.rt.Exec(r.Context(), id, wrapped, wantStdin)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			s.writeError(w, http.StatusNotFound, "no sprite "+id)
			return
		}
		s.writeError(w, http.StatusBadGateway, "exec: "+err.Error())
		return
	}
	defer func() { _ = proc.Close() }()

	w.Header().Set("sprite-capabilities", "control-ws")
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Warn("exec websocket accept failed", "id", id, "err", err)
		s.killExec(id, execID)
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(16 << 20)
	ctx := r.Context()

	var outputs sync.WaitGroup
	pump := func(stream byte, rd io.Reader) {
		defer outputs.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				if werr := writeFrame(ctx, c, stream, buf[:n]); werr != nil {
					_, _ = io.Copy(io.Discard, rd)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	outputs.Add(2)
	go pump(streamStdout, proc.Stdout())
	go pump(streamStderr, proc.Stderr())
	outDone := make(chan struct{})
	go func() { outputs.Wait(); close(outDone) }()

	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		stdinOpen := wantStdin
		if !wantStdin {
			_ = proc.Stdin().Close()
		}
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if len(data) == 0 || !stdinOpen {
				continue
			}
			switch data[0] {
			case streamStdin:
				if _, err := proc.Stdin().Write(data[1:]); err != nil {
					stdinOpen = false
				}
			case streamStdinEOF:
				_ = proc.Stdin().Close()
				stdinOpen = false
			}
		}
	}()

	select {
	case <-outDone:
	case <-clientGone:
		// The client left while the command runs: kill it in the sprite.
		s.killExec(id, execID)
		return
	}
	code, err := proc.Wait()
	go s.forgetExec(id, execID)
	if err != nil {
		_ = writeFrame(ctx, c, streamStderr, []byte("spritzer: "+err.Error()+"\n"))
		code = 255
	}
	if code < 0 || code > 255 {
		code = 255
	}
	if err := writeFrame(ctx, c, streamExit, []byte{byte(code)}); err != nil {
		return
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func (s *Server) killExec(name, execID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _, _, _ = runtime.Run(ctx, s.real.rt, name, []string{runtime.AgentBinary, "x-kill", execID}, nil)
	}()
}

func (s *Server) forgetExec(name, execID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, _, _ = runtime.Run(ctx, s.real.rt, name, []string{runtime.AgentBinary, "x-forget", execID}, nil)
}

// realServices forwards /v1/sprites/{id}/services/... to the agent's
// /v1/services/... on the sprite's management socket, verbatim, the way wispd
// forwards to its guest agent. The shapes are wisp's (PUT creates and starts,
// streaming NDJSON for ?duration).
func (s *Server) realServices(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	rest := "/v1/services"
	if tail := r.PathValue("rest"); tail != "" {
		rest += "/" + tail
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "sprite"
			pr.Out.URL.Path, pr.Out.URL.RawPath = rest, ""
			pr.Out.Host = "sprite"
		},
		Transport:     s.real.transport(id, "unix:"+runtime.AgentSocket),
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.writeError(w, http.StatusBadGateway, "sprite agent: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// proxySpriteURL serves the sprite URL: HTTP and WebSocket to the port the
// sprite publishes (the http_port service, else 8080). prefix is the path the
// sprite is mounted under (/s/<name>), passed on as X-Forwarded-Prefix.
func (s *Server) proxySpriteURL(w http.ResponseWriter, r *http.Request, name, path, prefix string) {
	if _, err := s.store.Get(name); err != nil {
		s.writeError(w, http.StatusNotFound, "no sprite "+name)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "sprite"
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			if prefix != "" {
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			}
		},
		Transport:     s.real.transport(name, "url"),
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Retry-After", "2")
			s.writeError(w, http.StatusServiceUnavailable, "sprite "+name+": nothing is answering on its URL port: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// ---- filesystem, through `spritzer x-fs` in the sprite ----

func (s *Server) fsRun(w http.ResponseWriter, r *http.Request, input []byte, args ...string) ([]byte, bool) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return nil, false
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		s.writeError(w, http.StatusBadRequest, "path is required")
		return nil, false
	}
	argv := append([]string{runtime.AgentBinary, "x-fs", args[0], path}, args[1:]...)
	out, stderr, code, err := runtime.Run(r.Context(), s.real.rt, id, argv, input)
	switch {
	case err != nil:
		s.writeError(w, http.StatusBadGateway, "fs: "+err.Error())
		return nil, false
	case code == 2:
		s.writeError(w, http.StatusNotFound, "no path "+path)
		return nil, false
	case code != 0:
		s.writeError(w, http.StatusInternalServerError, "fs: "+strings.TrimSpace(string(stderr)))
		return nil, false
	}
	return out, true
}

func (s *Server) realFSWrite(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if body == nil {
		body = []byte{}
	}
	args, err := fsWriteArgs(r.URL.Query())
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.fsRun(w, r, body, args...); ok {
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// fsWriteArgs builds the extra `x-fs write` args from the request's query: a
// `mode` (octal, e.g. "0755") rides as `--mode <value>`, applied to the file
// in the sprite container as the Sprites API and wisp do. Absent, the agent
// defaults to 0644.
func fsWriteArgs(q url.Values) ([]string, error) {
	args := []string{"write"}
	mode := q.Get("mode")
	if mode == "" {
		return args, nil
	}
	if _, err := strconv.ParseUint(mode, 8, 32); err != nil {
		return nil, fmt.Errorf("mode must be octal, e.g. 0755: %q", mode)
	}
	return append(args, "--mode", mode), nil
}

func (s *Server) realFSRead(w http.ResponseWriter, r *http.Request) {
	if out, ok := s.fsRun(w, r, nil, "read"); ok {
		writeRaw(w, http.StatusOK, out)
	}
}

func (s *Server) realFSList(w http.ResponseWriter, r *http.Request) {
	if out, ok := s.fsRun(w, r, nil, "list"); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

func (s *Server) realFSDelete(w http.ResponseWriter, r *http.Request) {
	args := []string{"delete"}
	if r.URL.Query().Get("recursive") == "true" {
		args = append(args, "--recursive")
	}
	if _, ok := s.fsRun(w, r, nil, args...); ok {
		writeJSON(w, http.StatusOK, struct{}{})
	}
}
