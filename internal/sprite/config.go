package sprite

import (
	"sort"
	"strings"
)

// This file adds the desired-state and runtime surfaces beyond the core
// lifecycle: a filesystem API, an outbound network policy, background services,
// and keep-alive tasks. They mirror the endpoints chant's in-process Sprites
// fake serves, so spritzer stays wire-compatible with it (see #855). Every
// method locks the store and treats a destroyed sprite as absent (ErrNotFound).

// NetworkRule is one outbound rule. Ordered — specificity is positional.
type NetworkRule struct {
	Domain string `json:"domain"`
	Action string `json:"action"`
}

// ServiceState is the live state projection of a background service.
type ServiceState struct {
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at,omitempty"`
}

// Service is a background service: its create config plus live state. Keyed by
// name; PUT is create-or-update.
type Service struct {
	Name     string            `json:"name"`
	Cmd      string            `json:"cmd"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Dir      string            `json:"dir,omitempty"`
	Needs    []string          `json:"needs,omitempty"`
	HTTPPort int               `json:"http_port,omitempty"`
	State    ServiceState      `json:"state"`
}

// Task is a keep-alive hold. While any task exists the sprite stays active.
type Task struct {
	Name   string `json:"name"`
	Expire any    `json:"expire,omitempty"`
}

// DirEntry is one filesystem listing entry. Type is "file" or "dir".
type DirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int    `json:"size,omitempty"`
}

// ---- filesystem ----

// WriteFile writes raw contents at path. It returns ErrNotFound for a missing or
// destroyed sprite.
func (s *Store) WriteFile(id, path, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return err
	}
	sp.FS[path] = content
	return nil
}

// ReadFile returns the contents at path and whether it exists. It returns
// ErrNotFound for a missing or destroyed sprite.
func (s *Store) ReadFile(id, path string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return "", false, err
	}
	v, ok := sp.FS[path]
	return v, ok, nil
}

// ListDir returns the immediate children of dir: a key "<dir>/name" is a file,
// "<dir>/name/..." contributes the dir "name" once. It returns ErrNotFound for a
// missing or destroyed sprite.
func (s *Store) ListDir(id, dir string) ([]DirEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimRight(dir, "/") + "/"
	files := map[string]int{}
	dirs := map[string]struct{}{}
	for key, val := range sp.FS {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := key[len(prefix):]
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i == -1 {
			files[rest] = len(val)
		} else {
			dirs[rest[:i]] = struct{}{}
		}
	}
	out := make([]DirEntry, 0, len(files)+len(dirs))
	dirNames := make([]string, 0, len(dirs))
	for name := range dirs {
		dirNames = append(dirNames, name)
	}
	sort.Strings(dirNames)
	for _, name := range dirNames {
		out = append(out, DirEntry{Name: name, Type: "dir"})
	}
	fileNames := make([]string, 0, len(files))
	for name := range files {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)
	for _, name := range fileNames {
		out = append(out, DirEntry{Name: name, Type: "file", Size: files[name]})
	}
	return out, nil
}

// Remove deletes path (recursively when recursive is set: path itself and every
// key under "<path>/"). It returns whether anything was removed, and ErrNotFound
// for a missing or destroyed sprite.
func (s *Store) Remove(id, path string, recursive bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return false, err
	}
	if recursive {
		prefix := strings.TrimRight(path, "/")
		removed := 0
		for key := range sp.FS {
			if key == prefix || strings.HasPrefix(key, prefix+"/") {
				delete(sp.FS, key)
				removed++
			}
		}
		return removed > 0, nil
	}
	if _, ok := sp.FS[path]; !ok {
		return false, nil
	}
	delete(sp.FS, path)
	return true, nil
}

// ---- network policy ----

// GetPolicy returns the sprite's outbound rules (a non-nil slice). It returns
// ErrNotFound for a missing or destroyed sprite.
func (s *Store) GetPolicy(id string) ([]NetworkRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return nil, err
	}
	return cloneRules(sp.NetPolicy), nil
}

// SetPolicy replaces the sprite's outbound rules (whole-object replace) and
// returns the applied set. It returns ErrNotFound for a missing or destroyed
// sprite.
func (s *Store) SetPolicy(id string, rules []NetworkRule) ([]NetworkRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return nil, err
	}
	sp.NetPolicy = cloneRules(rules)
	return cloneRules(sp.NetPolicy), nil
}

// ---- services ----

// ListServices returns the sprite's services in name order. It returns
// ErrNotFound for a missing or destroyed sprite.
func (s *Store) ListServices(id string) ([]Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sp.Services))
	for name := range sp.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Service, 0, len(names))
	for _, name := range names {
		out = append(out, *sp.Services[name])
	}
	return out, nil
}

// GetService returns a single service and whether it exists. It returns
// ErrNotFound for a missing or destroyed sprite.
func (s *Store) GetService(id, name string) (Service, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return Service{}, false, err
	}
	svc, ok := sp.Services[name]
	if !ok {
		return Service{}, false, nil
	}
	return *svc, true, nil
}

// PutService creates or updates a service by name, preserving its live state
// across an update. It returns ErrNotFound for a missing or destroyed sprite.
func (s *Store) PutService(id string, svc Service) (Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return Service{}, err
	}
	state := ServiceState{Name: svc.Name, Status: "stopped"}
	if prev, ok := sp.Services[svc.Name]; ok {
		state = prev.State
	}
	svc.State = state
	stored := svc
	sp.Services[svc.Name] = &stored
	return stored, nil
}

// SetServiceState applies a start/stop/restart action to a service, flipping its
// status, and returns the service and whether it exists. It returns ErrNotFound
// for a missing or destroyed sprite.
func (s *Store) SetServiceState(id, name, action string) (Service, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return Service{}, false, err
	}
	svc, ok := sp.Services[name]
	if !ok {
		return Service{}, false, nil
	}
	if action == "stop" {
		svc.State = ServiceState{Name: name, PID: 0, Status: "stopped"}
	} else {
		svc.State = ServiceState{Name: name, PID: 4321, Status: "running", StartedAt: s.clk.Now().UTC().Format("2006-01-02T15:04:05Z07:00")}
	}
	return *svc, true, nil
}

// ---- tasks ----

// CreateTask records a keep-alive hold. It returns ErrNotFound for a missing or
// destroyed sprite.
func (s *Store) CreateTask(id, name string, expire any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return err
	}
	sp.Tasks[name] = Task{Name: name, Expire: expire}
	return nil
}

// RefreshTask updates a task's expiry, returning whether it existed. It returns
// ErrNotFound for a missing or destroyed sprite.
func (s *Store) RefreshTask(id, name string, expire any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return false, err
	}
	t, ok := sp.Tasks[name]
	if !ok {
		return false, nil
	}
	if expire != nil {
		t.Expire = expire
	}
	sp.Tasks[name] = t
	return true, nil
}

// ReleaseTask removes a task, returning whether it existed. It returns
// ErrNotFound for a missing or destroyed sprite.
func (s *Store) ReleaseTask(id, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return false, err
	}
	if _, ok := sp.Tasks[name]; !ok {
		return false, nil
	}
	delete(sp.Tasks, name)
	return true, nil
}

// ListTasks returns the sprite's active tasks in name order. It returns
// ErrNotFound for a missing or destroyed sprite.
func (s *Store) ListTasks(id string) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sp.Tasks))
	for name := range sp.Tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Task, 0, len(names))
	for _, name := range names {
		out = append(out, sp.Tasks[name])
	}
	return out, nil
}

// cloneRules copies a ruleset so stored and returned values never share memory.
func cloneRules(rules []NetworkRule) []NetworkRule {
	out := make([]NetworkRule, len(rules))
	copy(out, rules)
	return out
}
