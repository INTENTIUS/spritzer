// Package sprite is a thread-safe, in-memory model of Sprites and their
// checkpoint/restore semantics. It owns the canonical state; callers receive
// copies so that concurrent reads and writes never share mutable memory.
//
// A sprite's filesystem is modeled as a path -> contents map. exec runs a small
// scripted interpreter (see exec.go) that can write or modify an fs key, so a
// checkpoint (a deep copy of the fs under a server-assigned version id) and a
// later restore (replace the fs with that copy) are observable. This mirrors the
// behavior of chant's in-process Sprites fake rather than real code execution.
//
// Checkpoints are addressed by a server-assigned version id (v1, v2, …), not by
// a caller label. The caller supplies only an optional comment; the store
// assigns the id sequentially per sprite in creation order.
package sprite

import (
	"errors"
	"fmt"
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
	// ErrCheckpointNotFound is returned by Restore for an unknown checkpoint id.
	ErrCheckpointNotFound = errors.New("checkpoint not found")
)

// Checkpoint is a captured filesystem snapshot: a server-assigned version id
// (v1, v2, …), the caller-supplied comment, the creation timestamp, whether it
// was created automatically, and a full copy of the fs at checkpoint time.
type Checkpoint struct {
	ID         string
	Comment    string
	CreateTime string
	IsAuto     bool
	FS         map[string]string
}

// CheckpointInfo is the metadata projection of a checkpoint, without its fs
// copy. It is what the list endpoint, the individual GET, and the sprite view
// expose so a client can pick a checkpoint by id (or, for compensation, by the
// newest matching comment). The JSON tags match the real Sprites checkpoint
// shape: id, comment, create_time, is_auto.
type CheckpointInfo struct {
	ID         string `json:"id"`
	Comment    string `json:"comment"`
	CreateTime string `json:"create_time"`
	IsAuto     bool   `json:"is_auto"`
}

// Sprite is a single sprite: its lifecycle status, its addressable URL, its
// filesystem, and its checkpoints (an ordered list, each a full copy of the fs
// at checkpoint time under a sequential v<N> id).
type Sprite struct {
	ID          string
	Status      Status
	URL         string
	FS          map[string]string
	Checkpoints []Checkpoint
	Policy      any
	// CreatedAt is stamped from the injected clock at creation. It is internal
	// bookkeeping and is not part of the wire contract.
	CreatedAt string
}

// ExecResult is the outcome of running a command in a sprite. The server's
// control-WebSocket exec handler carries these three fields to the client as
// framed messages: stdout as StreamStdout, stderr as StreamStderr, and the exit
// code as the StreamExit frame's single payload byte.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// View is the read-only projection returned by GET /v1/sprites/{id}: the
// checkpoints are exposed as an ordered list of {id, comment} projections, not
// their fs copies.
type View struct {
	ID          string            `json:"id"`
	Status      Status            `json:"status"`
	URL         string            `json:"url"`
	FS          map[string]string `json:"fs"`
	Checkpoints []CheckpointInfo  `json:"checkpoints"`
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
		Checkpoints: nil,
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

// Checkpoint deep-copies the sprite's current filesystem under a fresh,
// server-assigned version id and returns that id. Ids are assigned sequentially
// per sprite: "v1", "v2", …, one past the current checkpoint count. The caller
// controls only the comment, which may be empty. It returns ErrNotFound for a
// missing or destroyed sprite.
func (s *Store) Checkpoint(id, comment string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return "", err
	}
	cid := fmt.Sprintf("v%d", len(sp.Checkpoints)+1)
	sp.Checkpoints = append(sp.Checkpoints, Checkpoint{
		ID:         cid,
		Comment:    comment,
		CreateTime: s.clk.Now().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		IsAuto:     false,
		FS:         cloneFS(sp.FS),
	})
	return cid, nil
}

// GetCheckpoint returns a single checkpoint's metadata projection. It returns
// ErrNotFound for a missing or destroyed sprite, and ErrCheckpointNotFound for
// an unknown id.
func (s *Store) GetCheckpoint(id, checkpointID string) (CheckpointInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return CheckpointInfo{}, err
	}
	for i := range sp.Checkpoints {
		if sp.Checkpoints[i].ID == checkpointID {
			return checkpointInfo(sp.Checkpoints[i]), nil
		}
	}
	return CheckpointInfo{}, ErrCheckpointNotFound
}

// ListCheckpoints returns the sprite's checkpoints as {id, comment} projections
// in creation order (oldest first), so a client can pick the newest
// deterministically. It returns ErrNotFound for a missing or destroyed sprite.
func (s *Store) ListCheckpoints(id string) ([]CheckpointInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return nil, err
	}
	return checkpointInfos(sp.Checkpoints), nil
}

// Restore replaces the sprite's filesystem with the identified checkpoint's copy
// and sets its status back to running. The checkpoint is addressed by its
// server-assigned version id (v1, v2, …). It returns ErrNotFound for a missing
// or destroyed sprite, and ErrCheckpointNotFound for an unknown id.
func (s *Store) Restore(id, checkpointID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.live(id)
	if err != nil {
		return err
	}
	for i := range sp.Checkpoints {
		if sp.Checkpoints[i].ID == checkpointID {
			sp.FS = cloneFS(sp.Checkpoints[i].FS)
			sp.Status = StatusRunning
			return nil
		}
	}
	return ErrCheckpointNotFound
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
	return View{
		ID:          sp.ID,
		Status:      sp.Status,
		URL:         sp.URL,
		FS:          cloneFS(sp.FS),
		Checkpoints: checkpointInfos(sp.Checkpoints),
	}, nil
}

// checkpointInfos projects a checkpoint list to its metadata view, always
// returning a non-nil slice so it marshals as [] rather than null.
func checkpointInfos(cps []Checkpoint) []CheckpointInfo {
	out := make([]CheckpointInfo, 0, len(cps))
	for _, cp := range cps {
		out = append(out, checkpointInfo(cp))
	}
	return out
}

// checkpointInfo projects a single checkpoint to its metadata view.
func checkpointInfo(cp Checkpoint) CheckpointInfo {
	return CheckpointInfo{
		ID:         cp.ID,
		Comment:    cp.Comment,
		CreateTime: cp.CreateTime,
		IsAuto:     cp.IsAuto,
	}
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
		cps := make([]Checkpoint, len(sp.Checkpoints))
		for i, cp := range sp.Checkpoints {
			cps[i] = cp
			cps[i].FS = cloneFS(cp.FS)
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
