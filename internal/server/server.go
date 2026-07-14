// Package server wires the sprite store behind a net/http router that speaks the
// Sprites REST surface over the /v1 path space. It uses Go 1.22+ method+pattern
// routing so it needs no third-party router, and is wire-compatible with chant's
// in-process Sprites fake.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/intentius/spritzer/internal/clock"
	"github.com/intentius/spritzer/internal/sprite"
)

// implementedPaths backs the health/coverage endpoint.
var implementedPaths = []string{
	"POST /v1/sprites",
	"GET /v1/sprites/{id}/exec (control WebSocket)",
	"POST /v1/sprites/{id}/checkpoint",
	"GET /v1/sprites/{id}/checkpoints",
	"GET /v1/sprites/{id}/checkpoints/{cid}",
	"POST /v1/sprites/{id}/checkpoints/{cid}/restore",
	"DELETE /v1/sprites/{id}",
	"GET /v1/sprites/{id}",
	"GET /_spritzer/health",
}

// init appends the config/runtime endpoints (#855) to the coverage list.
func init() {
	implementedPaths = append(implementedPaths, configPaths...)
}

// Options configures a Server.
type Options struct {
	Version string
	Clock   clock.Clock
	Logger  *slog.Logger
}

// Server holds the spritzer state and serves the API.
type Server struct {
	version string
	log     *slog.Logger
	store   *sprite.Store
	clock   clock.Clock
	mux     *http.ServeMux
}

// New constructs a Server, filling in sensible defaults for any zero option.
func New(opts Options) *Server {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	s := &Server{
		version: opts.Version,
		log:     opts.Logger,
		store:   sprite.New(opts.Clock),
		clock:   opts.Clock,
	}
	s.routes()
	return s
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/sprites", s.createSprite)
	mux.HandleFunc("GET /v1/sprites/{id}/exec", s.execSpriteWS)
	mux.HandleFunc("POST /v1/sprites/{id}/checkpoint", s.checkpointSprite)
	mux.HandleFunc("GET /v1/sprites/{id}/checkpoints", s.listCheckpoints)
	mux.HandleFunc("GET /v1/sprites/{id}/checkpoints/{cid}", s.getCheckpoint)
	mux.HandleFunc("POST /v1/sprites/{id}/checkpoints/{cid}/restore", s.restoreCheckpoint)
	mux.HandleFunc("DELETE /v1/sprites/{id}", s.destroySprite)
	mux.HandleFunc("GET /v1/sprites/{id}", s.getSprite)

	// Filesystem, network policy, services, and keep-alive tasks (#855).
	s.configRoutes(mux)

	mux.HandleFunc("GET /_spritzer/health", s.health)

	s.mux = mux
}

// ---- request/response wire types ----

// createRequest is the body of POST /v1/sprites.
type createRequest struct {
	Name   string `json:"name"`
	Image  string `json:"image,omitempty"`
	Size   string `json:"size,omitempty"`
	Policy any    `json:"policy,omitempty"`
}

// createResponse is the POST /v1/sprites response.
type createResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// checkpointRequest is the body of POST /v1/sprites/{id}/checkpoint. The caller
// supplies only an optional comment; the checkpoint id is server-assigned.
type checkpointRequest struct {
	Comment string `json:"comment,omitempty"`
}

// progressEvent is one line of the NDJSON progress stream that the checkpoint
// create and restore endpoints emit, mirroring the real Sprites API's
// line-delimited progress body: each line is tagged with a "type" ("info" or
// "complete") and a human "data" message. There is no structured id field — the
// version id rides inside the message text ("  ID: v1", "Checkpoint v1 created
// successfully"), exactly as api.sprites.dev streams it.
type progressEvent struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Time string `json:"time,omitempty"`
}

// info builds an "info" progress line stamped with the server clock.
func (s *Server) info(data string) progressEvent {
	return progressEvent{Type: "info", Data: data, Time: s.clock.Now().UTC().Format(time.RFC3339Nano)}
}

// complete builds the terminal "complete" progress line stamped with the clock.
func (s *Server) complete(data string) progressEvent {
	return progressEvent{Type: "complete", Data: data, Time: s.clock.Now().UTC().Format(time.RFC3339Nano)}
}

// ErrorResponse is the JSON body spritzer returns for any non-2xx status. It
// carries an "error" message and a machine-readable status.
type ErrorResponse struct {
	Error  string `json:"error"`
	Status int    `json:"status,omitempty"`
}

// ---- handlers ----

func (s *Server) createSprite(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	created := s.store.Create(req.Name, spriteURL(r.Host, req.Name), req.Policy)
	writeJSON(w, http.StatusCreated, createResponse{ID: created.ID, URL: created.URL})
}

// checkpointSprite creates a checkpoint and streams NDJSON progress events. The
// store assigns the version id (v1, v2, …); the stream reports it in the message
// text ("  ID: v1" and the terminal "Checkpoint v1 created successfully"),
// matching real Sprites, which carries no structured id field. The sprite lookup
// error is resolved before the stream starts so an unknown sprite is a plain
// 404 rather than a half-written stream.
func (s *Server) checkpointSprite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req checkpointRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	cid, err := s.store.Checkpoint(id, req.Comment)
	if s.handleLookupError(w, id, err) {
		return
	}
	writeNDJSON(w, []progressEvent{
		s.info("Creating checkpoint..."),
		s.info("Checkpoint created successfully"),
		s.info("  ID: " + cid),
		s.info("  Path: checkpoints/" + cid),
		s.complete("Checkpoint " + cid + " created successfully"),
	})
}

func (s *Server) listCheckpoints(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cps, err := s.store.ListCheckpoints(id)
	if s.handleLookupError(w, id, err) {
		return
	}
	// The list is a bare JSON array, not wrapped in an object.
	writeJSON(w, http.StatusOK, cps)
}

func (s *Server) getCheckpoint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	info, err := s.store.GetCheckpoint(id, cid)
	if errors.Is(err, sprite.ErrCheckpointNotFound) {
		s.writeError(w, http.StatusNotFound, "no checkpoint \""+cid+"\" for sprite "+id)
		return
	}
	if s.handleLookupError(w, id, err) {
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// restoreCheckpoint replaces the sprite's filesystem with the identified
// checkpoint's copy and streams NDJSON progress events. As with create, the
// error is resolved before streaming so an unknown sprite or checkpoint is a
// plain 404.
func (s *Server) restoreCheckpoint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	err := s.store.Restore(id, cid)
	if errors.Is(err, sprite.ErrCheckpointNotFound) {
		s.writeError(w, http.StatusNotFound, "no checkpoint \""+cid+"\" for sprite "+id)
		return
	}
	if s.handleLookupError(w, id, err) {
		return
	}
	writeNDJSON(w, []progressEvent{
		s.info("Restoring checkpoint " + cid + "..."),
		s.complete("Checkpoint " + cid + " restored successfully"),
	})
}

func (s *Server) destroySprite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.handleLookupError(w, id, s.store.Destroy(id)) {
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) getSprite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	view, err := s.store.Get(id)
	if s.handleLookupError(w, id, err) {
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"version":     s.version,
		"implemented": implementedPaths,
	})
}

// ---- helpers ----

// spriteURL builds a sprite's addressable URL from the request host, matching
// the fake's `http://<host>/s/<id>` shape.
func spriteURL(host, id string) string {
	if host == "" {
		host = "localhost"
	}
	return "http://" + host + "/s/" + url.PathEscape(id)
}

// handleLookupError writes a 404 for a missing/destroyed sprite and reports
// whether it wrote a response.
func (s *Server) handleLookupError(w http.ResponseWriter, id string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sprite.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "no sprite "+id)
	default:
		s.writeError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg, Status: status})
}

// decodeJSON decodes an optional JSON body. An empty body decodes to the zero
// value, which is valid for the endpoints that accept optional bodies.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	s.writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeNDJSON streams a sequence of progress events as line-delimited JSON with
// Content-Type application/x-ndjson, flushing after each line so a client
// consuming the stream sees progress before completion. json.Encoder.Encode
// appends the newline that delimits each event.
func writeNDJSON(w http.ResponseWriter, events []progressEvent) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	for i := range events {
		_ = enc.Encode(events[i])
		if flusher != nil {
			flusher.Flush()
		}
	}
}
