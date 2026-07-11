// Package sprite is a thread-safe, in-memory model of Sprites and their
// checkpoint/restore semantics. It owns the canonical state; callers receive
// copies so that concurrent reads and writes never share mutable memory.
//
// A sprite's filesystem is modeled as a path -> contents map. exec runs a small
// scripted interpreter (see exec.go) that can write or modify an fs key, so a
// checkpoint (a deep copy of the fs under a label) and a later restore (replace
// the fs with that copy) are observable. This mirrors the behavior of chant's
// in-process Sprites fake rather than real code execution.
package sprite

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/intentius/spritzer/internal/clock"
)

// Status is the lifecycle state of a sprite.
type Status string

// The sprite lifecycle states. A sprite is created running; destroy moves it to
// destroyed, after which every operation on it reports ErrNotFound.
const (
	StatusStarting  Status = "starting"
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusDestroyed Status = "destroyed"
)

// Sentinel errors returned by the store.
var (
	// ErrNotFound is returned for a sprite that was never created or has been
	// destroyed. A destroyed sprite is treated as absent, matching the fake.
	ErrNotFound = errors.New("sprite not found")
	// ErrCheckpointNotFound is returned by Restore for an unknown label.
	ErrCheckpointNotFound = errors.New("checkpoint not found")
)

// Sprite is a single sprite: its lifecycle status, its addressable URL, its
// filesystem, and its checkpoints (each a full copy of the fs at checkpoint
// time, keyed by label).
type Sprite struct {
	ID          string
	Status      Status
	URL         string
	FS          map[string]string
	Checkpoints map[string]map[string]string
	Policy      any
	// CreatedAt is stamped from the injected clock at creation. It is internal
	// bookkeeping and is not part of the wire contract.
	CreatedAt string
}

// ExecResult is the outcome of running a command in a sprite. The JSON tags
// match the Sprites exec response (note the camelCase exitCode).
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// View is the read-only projection returned by GET /v1/sprites/{id}: the
// checkpoints are exposed as a sorted list of labels, not their fs copies.
type View struct {
	ID          string            `json:"id"`
	Status      Status            `json:"status"`
	URL         string            `json:"url"`
	FS          map[string]string `json:"fs"`
	Checkpoints []string          `json:"checkpoints"`
}

// Store holds sprites keyed by id.
type Store struct {
	mu      sync.Mutex
	clk     clock.Clock
	sprites map[string]*Sprite
}

// New returns an empty store. A nil clock is replaced with a real one.
func New(clk clock.Clock) *Store {
	if clk == nil {
		clk = clock.Real()
	}
	return &Store{clk: clk, sprites: make(map[string]*Sprite)}
}

// Create records a new running sprite under id, with an empty filesystem and no
// checkpoints. Like the fake, it overwrites any existing sprite with the same
// id. It returns a copy of the created sprite.
func (s *Store) Create(id, url string, policy any) Sprite {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp := &Sprite{
		ID:          id,
		Status:      StatusRunning,
		URL:         url,
		FS:          map[string]string{},
		Checkpoints: map[string]map[string]string{},
		Policy:      policy,
		CreatedAt:   s.clk.Now().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
	s.sprites[id] = sp
	return *cloneSprite(sp)
}

// Exec runs cmd against the sprite's filesystem and returns the result. It
// returns ErrNotFound for a missing or destroyed sprite.
func (s *Store) Exec(id, cmd string) (ExecResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return ExecResult{}, err
	}
	return execInto(sp, cmd), nil
}

// Checkpoint deep-copies the sprite's current filesystem under a label and
// returns the checkpoint id. An empty label defaults to "cp-<n>", where n is one
// past the current checkpoint count. It returns ErrNotFound for a missing or
// destroyed sprite.
func (s *Store) Checkpoint(id, label string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return "", err
	}
	if label == "" {
		label = fmt.Sprintf("cp-%d", len(sp.Checkpoints)+1)
	}
	sp.Checkpoints[label] = cloneFS(sp.FS)
	return label, nil
}

// Restore replaces the sprite's filesystem with the labeled checkpoint's copy
// and sets its status back to running. It returns ErrNotFound for a missing or
// destroyed sprite, and ErrCheckpointNotFound for an unknown label.
func (s *Store) Restore(id, checkpoint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return err
	}
	snap, ok := sp.Checkpoints[checkpoint]
	if !ok {
		return ErrCheckpointNotFound
	}
	sp.FS = cloneFS(snap)
	sp.Status = StatusRunning
	return nil
}

// Destroy marks the sprite destroyed. It returns ErrNotFound if the sprite is
// missing or already destroyed.
func (s *Store) Destroy(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return err
	}
	sp.Status = StatusDestroyed
	return nil
}

// Get returns a read-only view of the sprite. It returns ErrNotFound for a
// missing or destroyed sprite.
func (s *Store) Get(id string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return View{}, err
	}
	labels := make([]string, 0, len(sp.Checkpoints))
	for l := range sp.Checkpoints {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	return View{
		ID:          sp.ID,
		Status:      sp.Status,
		URL:         sp.URL,
		FS:          cloneFS(sp.FS),
		Checkpoints: labels,
	}, nil
}

// live finds a sprite without locking (callers hold s.mu). A destroyed sprite is
// reported as ErrNotFound so every op past destroy behaves as if it is gone.
func (s *Store) live(id string) (*Sprite, error) {
	sp, ok := s.sprites[id]
	if !ok || sp.Status == StatusDestroyed {
		return nil, ErrNotFound
	}
	return sp, nil
}

// cloneSprite deep-copies a sprite so stored and returned values never share
// mutable maps.
func cloneSprite(sp *Sprite) *Sprite {
	c := *sp
	c.FS = cloneFS(sp.FS)
	if sp.Checkpoints != nil {
		cps := make(map[string]map[string]string, len(sp.Checkpoints))
		for label, fs := range sp.Checkpoints {
			cps[label] = cloneFS(fs)
		}
		c.Checkpoints = cps
	}
	return &c
}

// cloneFS returns a shallow copy of an fs map (values are strings).
func cloneFS(fs map[string]string) map[string]string {
	out := make(map[string]string, len(fs))
	for k, v := range fs {
		out[k] = v
	}
	return out
}
