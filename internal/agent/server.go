//go:build !windows

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Run is `spritzer agent`: the sprite's main process. It starts the services
// defined on disk and serves the management socket until SIGTERM.
func Run(p Paths, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	for _, d := range []string{p.ServicesDir(), p.LogsDir(), p.ExecDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	sv := NewSupervisor(p)
	ln, err := Listen(p.Socket())
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: Handler(sv), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	logger.Info("spritzer agent ready", "socket", p.Socket(), "services", len(sv.List()))

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	sv.StopAll(3 * time.Second)
	return nil
}

// Listen creates the management socket, replacing a stale one.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o666)
	return ln, nil
}

// Handler is the management API on /.sprite/api.sock: services and tasks, in
// wisp's guest API shapes. Checkpoints from inside are refused, since container
// mode has no checkpoints.
func Handler(sv *Supervisor) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_agent/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /v1/services", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, sv.List())
	})
	mux.HandleFunc("GET /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		svc, err := sv.Get(r.PathValue("name"))
		if err != nil {
			serviceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, svc)
	})
	mux.HandleFunc("PUT /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		var def ServiceDef
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&def); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		def.Name = r.PathValue("name")
		if err := sv.Define(def); err != nil {
			serviceErr(w, err)
			return
		}
		streamAction(sv, w, r, true, sv.Start)
	})
	mux.HandleFunc("DELETE /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := sv.Delete(r.PathValue("name")); err != nil {
			serviceErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/services/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		streamAction(sv, w, r, true, sv.Start)
	})
	mux.HandleFunc("POST /v1/services/{name}/restart", func(w http.ResponseWriter, r *http.Request) {
		streamAction(sv, w, r, true, func(n string) error { return sv.Restart(n, stopTimeout(r)) })
	})
	mux.HandleFunc("POST /v1/services/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		streamAction(sv, w, r, false, func(n string) error { return sv.Stop(n, stopTimeout(r)) })
	})
	mux.HandleFunc("POST /v1/services/signal", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name, Signal string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		sig, err := ParseSignal(req.Signal)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if err := sv.Signal(req.Name, sig); err != nil {
			serviceErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/services/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		serviceLogs(sv, w, r)
	})

	tasks := &taskTable{m: map[string]Task{}}
	tasks.register(mux)

	refuse := func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotImplemented, "not_implemented",
			"checkpoints are not available in spritzer's container exec mode")
	}
	mux.HandleFunc("/v1/checkpoint", refuse)
	mux.HandleFunc("/v1/checkpoints", refuse)
	mux.HandleFunc("/v1/checkpoints/", refuse)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr uses wisp's error shape; sprite-env prints "message".
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": code, "message": msg})
}

func serviceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrServiceNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrServiceConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

func stopTimeout(r *http.Request) time.Duration {
	if d, err := time.ParseDuration(r.URL.Query().Get("timeout")); err == nil && d > 0 {
		return d
	}
	return defaultStopTimeout
}

// streamAction runs a lifecycle action and streams the service's events as
// NDJSON. When watch is set it keeps streaming for ?duration (default 5s) after
// the action, so the caller sees whether the service stayed up.
func streamAction(sv *Supervisor, w http.ResponseWriter, r *http.Request, watch bool, action func(string) error) {
	name := r.PathValue("name")
	watchFor := 5 * time.Second
	if v := r.URL.Query().Get("duration"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid duration")
			return
		}
		watchFor = d
	}
	events, cancel, err := sv.Subscribe(name)
	if err != nil {
		serviceErr(w, err)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(ev ServiceEvent) {
		if ev.Timestamp == 0 {
			ev.Timestamp = time.Now().UnixMilli()
		}
		_ = enc.Encode(ev)
		if fl != nil {
			fl.Flush()
		}
	}
	acted := make(chan error, 1)
	go func() { acted <- action(name) }()
	var deadline <-chan time.Time
	done := false
loop:
	for {
		select {
		case ev := <-events:
			send(ev)
		case err := <-acted:
			done = true
			if err != nil {
				send(ServiceEvent{Type: "error", Data: err.Error()})
				break loop
			}
			if !watch || watchFor <= 0 {
				break loop
			}
			deadline = time.After(watchFor)
		case <-deadline:
			break loop
		case <-r.Context().Done():
			return
		}
	}
	for drained := false; !drained && done; {
		select {
		case ev := <-events:
			send(ev)
		default:
			drained = true
		}
	}
	send(ServiceEvent{Type: "complete", LogFiles: map[string]string{"combined": sv.LogPath(name)}})
}

// serviceLogs returns the tail of a service's log as NDJSON.
func serviceLogs(sv *Supervisor, w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := sv.Get(name); err != nil {
		serviceErr(w, err)
		return
	}
	lines := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && n > 0 {
		lines = n
	}
	var tail []string
	if f, err := os.Open(sv.LogPath(name)); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			if tail = append(tail, sc.Text()); len(tail) > lines {
				tail = tail[1:]
			}
		}
		_ = f.Close()
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	for _, l := range tail {
		ev := ServiceEvent{Type: "stdout", Data: l}
		if ts, rest, ok := strings.Cut(l, " ["); ok {
			if stream, text, ok := strings.Cut(rest, "] "); ok {
				ev.Type, ev.Data = stream, text
				if t, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err == nil {
					ev.Timestamp = t.UnixMilli()
				}
			}
		}
		_ = enc.Encode(ev)
	}
	_ = enc.Encode(ServiceEvent{Type: "complete", Timestamp: time.Now().UnixMilli(),
		LogFiles: map[string]string{"combined": sv.LogPath(name)}})
}

// Task is a keep-awake hold. Container sprites never sleep, so tasks are only
// recorded, but a box's keep-awake loop must get a 2xx for its PUT.
type Task struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type taskTable struct {
	mu sync.Mutex
	m  map[string]Task
}

func (t *taskTable) live() []Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []Task{}
	for n, task := range t.m {
		if !time.Now().Before(task.ExpiresAt) {
			delete(t.m, n)
			continue
		}
		out = append(out, task)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (t *taskTable) put(name string, expire time.Duration) Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	task, ok := t.m[name]
	if !ok || !now.Before(task.ExpiresAt) {
		task = Task{Name: name, StartedAt: now}
	}
	task.ExpiresAt = now.Add(expire)
	t.m[name] = task
	return task
}

func (t *taskTable) register(mux *http.ServeMux) {
	type body struct {
		Name   string `json:"name"`
		Expire string `json:"expire"`
	}
	decode := func(w http.ResponseWriter, r *http.Request) (body, time.Duration, bool) {
		var b body
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b); err != nil {
				writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
				return b, 0, false
			}
		}
		d := 5 * time.Minute
		if b.Expire != "" {
			v, err := time.ParseDuration(b.Expire)
			if err != nil || v <= 0 {
				writeErr(w, http.StatusBadRequest, "bad_request", "invalid expire")
				return b, 0, false
			}
			d = v
		}
		return b, d, true
	}
	mux.HandleFunc("GET /v1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, t.live())
	})
	upsert := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, d, ok := decode(w, r)
			if !ok {
				return
			}
			if n := r.PathValue("name"); n != "" {
				b.Name = n
			}
			if b.Name == "" {
				writeErr(w, http.StatusBadRequest, "bad_request", "name is required")
				return
			}
			writeJSON(w, status, t.put(b.Name, d))
		}
	}
	mux.HandleFunc("POST /v1/tasks", upsert(http.StatusCreated))
	mux.HandleFunc("PUT /v1/tasks", upsert(http.StatusOK))
	mux.HandleFunc("PUT /v1/tasks/{name}", upsert(http.StatusOK))
	mux.HandleFunc("DELETE /v1/tasks/{name}", func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		_, ok := t.m[r.PathValue("name")]
		delete(t.m, r.PathValue("name"))
		t.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "not_found", "no such task")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
