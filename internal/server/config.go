package server

import (
	"io"
	"net/http"
	"time"

	"github.com/intentius/spritzer/internal/sprite"
)

// This file serves the desired-state and runtime surfaces beyond the core
// lifecycle: a filesystem API (raw-body read/write), an outbound network policy
// (whole-object replace), background services (create-or-update by name +
// start/stop/restart), and keep-alive tasks. They mirror chant's in-process
// Sprites fake so the two stay wire-compatible (see #855).

// configPaths are added to the health/coverage list.
var configPaths = []string{
	"PUT /v1/sprites/{id}/fs/write",
	"GET /v1/sprites/{id}/fs/read",
	"GET /v1/sprites/{id}/fs/list",
	"DELETE /v1/sprites/{id}/fs/delete",
	"GET /v1/sprites/{id}/policy/network",
	"POST /v1/sprites/{id}/policy/network",
	"GET /v1/sprites/{id}/services",
	"GET /v1/sprites/{id}/services/{svc}",
	"PUT /v1/sprites/{id}/services/{svc}",
	"POST /v1/sprites/{id}/services/{svc}/start",
	"POST /v1/sprites/{id}/services/{svc}/stop",
	"POST /v1/sprites/{id}/services/{svc}/restart",
	"GET /v1/sprites/{id}/tasks",
	"POST /v1/sprites/{id}/tasks",
	"PUT /v1/sprites/{id}/tasks/{name}",
	"DELETE /v1/sprites/{id}/tasks/{name}",
}

// configRoutes registers the config/runtime endpoints on the mux.
func (s *Server) configRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/sprites/{id}/fs/write", s.fsWrite)
	mux.HandleFunc("GET /v1/sprites/{id}/fs/read", s.fsRead)
	mux.HandleFunc("GET /v1/sprites/{id}/fs/list", s.fsList)
	mux.HandleFunc("DELETE /v1/sprites/{id}/fs/delete", s.fsDelete)

	mux.HandleFunc("GET /v1/sprites/{id}/policy/network", s.getPolicy)
	mux.HandleFunc("POST /v1/sprites/{id}/policy/network", s.setPolicy)

	mux.HandleFunc("GET /v1/sprites/{id}/services", s.listServices)
	mux.HandleFunc("GET /v1/sprites/{id}/services/{svc}", s.getService)
	mux.HandleFunc("PUT /v1/sprites/{id}/services/{svc}", s.putService)
	mux.HandleFunc("POST /v1/sprites/{id}/services/{svc}/start", s.serviceAction("start"))
	mux.HandleFunc("POST /v1/sprites/{id}/services/{svc}/stop", s.serviceAction("stop"))
	mux.HandleFunc("POST /v1/sprites/{id}/services/{svc}/restart", s.serviceAction("restart"))

	mux.HandleFunc("GET /v1/sprites/{id}/tasks", s.listTasks)
	mux.HandleFunc("POST /v1/sprites/{id}/tasks", s.createTask)
	mux.HandleFunc("PUT /v1/sprites/{id}/tasks/{name}", s.refreshTask)
	mux.HandleFunc("DELETE /v1/sprites/{id}/tasks/{name}", s.releaseTask)
}

// ---- filesystem ----

func (s *Server) fsWrite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		s.writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if s.handleLookupError(w, id, s.store.WriteFile(id, path, string(body))) {
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) fsRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		s.writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	content, ok, err := s.store.ReadFile(id, path)
	if s.handleLookupError(w, id, err) {
		return
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "no file "+path)
		return
	}
	writeRaw(w, http.StatusOK, []byte(content))
}

func (s *Server) fsList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		s.writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	entries, err := s.store.ListDir(id, path)
	if s.handleLookupError(w, id, err) {
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) fsDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		s.writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	recursive := r.URL.Query().Get("recursive") == "true"
	removed, err := s.store.Remove(id, path, recursive)
	if s.handleLookupError(w, id, err) {
		return
	}
	if !removed {
		s.writeError(w, http.StatusNotFound, "no path "+path)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// ---- network policy ----

type policyBody struct {
	Rules []sprite.NetworkRule `json:"rules"`
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rules, err := s.store.GetPolicy(id)
	if s.handleLookupError(w, id, err) {
		return
	}
	writeJSON(w, http.StatusOK, policyBody{Rules: rules})
}

func (s *Server) setPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req policyBody
	if !s.decodeJSON(w, r, &req) {
		return
	}
	rules, err := s.store.SetPolicy(id, req.Rules)
	if s.handleLookupError(w, id, err) {
		return
	}
	writeJSON(w, http.StatusOK, policyBody{Rules: rules})
}

// ---- services ----

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svcs, err := s.store.ListServices(id)
	if s.handleLookupError(w, id, err) {
		return
	}
	writeJSON(w, http.StatusOK, svcs)
}

func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("svc")
	svc, ok, err := s.store.GetService(id, name)
	if s.handleLookupError(w, id, err) {
		return
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "no service "+name)
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) putService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("svc")
	var svc sprite.Service
	if !s.decodeJSON(w, r, &svc) {
		return
	}
	svc.Name = name
	stored, err := s.store.PutService(id, svc)
	if s.handleLookupError(w, id, err) {
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// serviceAction returns a handler for start/stop/restart, streaming NDJSON.
func (s *Server) serviceAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		name := r.PathValue("svc")
		_, ok, err := s.store.SetServiceState(id, name, action)
		if s.handleLookupError(w, id, err) {
			return
		}
		if !ok {
			s.writeError(w, http.StatusNotFound, "no service "+name)
			return
		}
		lead := "started"
		if action == "stop" {
			lead = "stopping"
		}
		writeNDJSON(w, []progressEvent{
			{Type: lead, Data: name + " " + lead, Time: s.clock.Now().UTC().Format(time.RFC3339Nano)},
			s.complete(name + " " + action + " complete"),
		})
	}
}

// ---- tasks ----

type taskBody struct {
	Name   string `json:"name,omitempty"`
	Expire any    `json:"expire,omitempty"`
}

type taskResponse struct {
	Name      string `json:"name"`
	ExpiresAt string `json:"expires_at"`
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tasks, err := s.store.ListTasks(id)
	if s.handleLookupError(w, id, err) {
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req taskBody
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if s.handleLookupError(w, id, s.store.CreateTask(id, req.Name, req.Expire)) {
		return
	}
	writeJSON(w, http.StatusCreated, taskResponse{Name: req.Name, ExpiresAt: s.expiresAt()})
}

func (s *Server) refreshTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	var req taskBody
	if !s.decodeJSON(w, r, &req) {
		return
	}
	ok, err := s.store.RefreshTask(id, name, req.Expire)
	if s.handleLookupError(w, id, err) {
		return
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "no task "+name)
		return
	}
	writeJSON(w, http.StatusOK, taskResponse{Name: name, ExpiresAt: s.expiresAt()})
}

func (s *Server) releaseTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	ok, err := s.store.ReleaseTask(id, name)
	if s.handleLookupError(w, id, err) {
		return
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "no task "+name)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// expiresAt is a clock-stamped ISO timestamp for a task response.
func (s *Server) expiresAt() string {
	return s.clock.Now().UTC().Format(time.RFC3339)
}

// writeRaw writes raw bytes with an octet-stream content type (for fs reads).
func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
