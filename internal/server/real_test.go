package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/intentius/spritzer/internal/runtime"
)

// stubRuntime records calls; its Exec always fails, which is enough for the
// routes that do not reach into a sprite.
type stubRuntime struct {
	created, deleted []string
}

func (s *stubRuntime) Kind() string { return "stub" }
func (s *stubRuntime) Create(_ context.Context, name string) error {
	s.created = append(s.created, name)
	return nil
}
func (s *stubRuntime) Delete(_ context.Context, name string) error {
	for _, n := range s.created {
		if n == name {
			s.deleted = append(s.deleted, name)
			return nil
		}
	}
	return runtime.ErrNotFound
}
func (s *stubRuntime) List(context.Context) ([]string, error) { return s.created, nil }
func (s *stubRuntime) Exec(context.Context, string, []string, bool) (runtime.Process, error) {
	return nil, errors.New("stub has no exec")
}

func TestExecArgv(t *testing.T) {
	cases := []struct {
		q    url.Values
		want []string
	}{
		{url.Values{"cmd": {"uname", "-a"}}, []string{"uname", "-a"}},
		{url.Values{"cmd": {"echo hi > /tmp/x"}}, []string{"/bin/sh", "-c", "echo hi > /tmp/x"}},
		{url.Values{"cmd": {"true"}}, []string{"true"}},
		{url.Values{"path": {"/bin/ls"}}, []string{"/bin/ls"}},
		{url.Values{}, nil},
	}
	for _, c := range cases {
		if got := execArgv(c.q); !reflect.DeepEqual(got, c.want) {
			t.Errorf("execArgv(%v) = %q, want %q", c.q, got, c.want)
		}
	}
}

func TestContainerModeRoutes(t *testing.T) {
	rt := &stubRuntime{}
	srv := New(Options{Runtime: rt, URLDomain: "localhost"})
	h := srv.Handler()
	do := func(method, path, body, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if host != "" {
			req.Host = host
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	var health map[string]any
	_ = json.Unmarshal(do("GET", "/_spritzer/health", "", "").Body.Bytes(), &health)
	if health["exec"] != "container" || health["runtime"] != "stub" {
		t.Fatalf("health: %v", health)
	}

	if rec := do("POST", "/v1/sprites", `{"name":"Not_A_Label"}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name: %d %s", rec.Code, rec.Body)
	}
	rec := do("POST", "/v1/sprites", `{"name":"box"}`, "spritzer.test:4290")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"url":"http://box.localhost:4290"`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/v1/sprites", "", ""); !strings.Contains(rec.Body.String(), `"name":"box"`) {
		t.Fatalf("list: %s", rec.Body)
	}
	if rec := do("POST", "/v1/sprites/box/checkpoint", "", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("checkpoint in container mode: %d", rec.Code)
	}
	if rec := do("GET", "/v1/sprites/nope/services", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("services of a missing sprite: %d", rec.Code)
	}
	// The sprite URL answers 503 while nothing can be reached behind it.
	if rec := do("GET", "/anything", "", "box.localhost:4290"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("host-routed URL with a failing exec: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/s/box", "", ""); rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("/s/box: %d", rec.Code)
	}
	if rec := do("DELETE", "/v1/sprites/box", "", ""); rec.Code != http.StatusOK || len(rt.deleted) != 1 {
		t.Fatalf("destroy: %d %v", rec.Code, rt.deleted)
	}
	if rec := do("GET", "/v1/sprites/box", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get after destroy: %d", rec.Code)
	}
}

func TestInterpreterModeHasNoContainerRoutes(t *testing.T) {
	h := New(Options{}).Handler()
	for _, path := range []string{"/v1/sprites", "/s/box/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("interpreter mode serves %s", path)
		}
	}
}
