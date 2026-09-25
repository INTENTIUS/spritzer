// Package runtime runs sprites as containers for spritzer's container exec
// mode. A Backend creates, lists and deletes one container per sprite and
// execs commands in it. Two backends exist: Docker (the Engine API over its
// socket) and Kubernetes (one pod per sprite, in spritzer's namespace).
//
// Everything above a backend goes through Exec. The in-sprite agent
// (internal/agent) turns an exec into a socket connection or a file operation,
// so a backend needs no port forwarding and no network path into the sprite.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sync"
	"time"
)

// Well-known paths inside every sprite. The agent volume is mounted at BinDir.
const (
	BinDir      = "/.sprite/bin"
	AgentBinary = "/.sprite/bin/spritzer"
	AgentSocket = "/.sprite/api.sock"
)

// Labels every sprite container or pod carries.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelSprite    = "spritzer.intentius.io/sprite"
	ManagedBy      = "spritzer"
)

// ErrNotFound is a sprite the backend has no container for.
var ErrNotFound = errors.New("sprite container not found")

// nameRE is what a sprite name must be in container mode: a DNS label, since
// it becomes a pod name and the sprite's hostname.
var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,53}[a-z0-9])?$`)

// ValidName reports whether name can be a sprite in container mode.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("sprite name %q must be a DNS label (lowercase letters, digits and '-', at most 55 characters) in container exec mode", name)
	}
	return nil
}

// Config is what both backends need.
type Config struct {
	// SpriteImage is the image every sprite runs.
	SpriteImage string
	// AgentImage is spritzer's own image, which carries the Linux agent binary
	// copied into each sprite.
	AgentImage string
	// CreateTimeout bounds a create, image pulls included.
	CreateTimeout time.Duration
}

// Backend is one container runtime.
type Backend interface {
	// Kind is "docker" or "kubernetes".
	Kind() string
	// Create starts a sprite container and returns once its agent answers.
	// Creating a sprite that already exists is not an error.
	Create(ctx context.Context, name string) error
	// Delete removes the container and returns once it is gone.
	Delete(ctx context.Context, name string) error
	// List returns the names of every sprite container the backend holds.
	List(ctx context.Context) ([]string, error)
	// Exec starts argv in the sprite. With stdin false the process's stdin is
	// closed from the start.
	Exec(ctx context.Context, name string, argv []string, stdin bool) (Process, error)
}

// Process is a running exec. Stdout and Stderr must both be read, or the
// stream stalls. Wait returns the exit code once both have reached EOF.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() (int, error)
	// Close abandons the stream. It does not kill the process inside the
	// sprite; see agent.Kill.
	Close() error
}

// Run execs argv with input as stdin and collects the output.
func Run(ctx context.Context, b Backend, name string, argv []string, input []byte) (stdout, stderr []byte, code int, err error) {
	p, err := b.Exec(ctx, name, argv, input != nil)
	if err != nil {
		return nil, nil, -1, err
	}
	defer func() { _ = p.Close() }()
	if input != nil {
		go func() {
			_, _ = p.Stdin().Write(input)
			_ = p.Stdin().Close()
		}()
	}
	var wg sync.WaitGroup
	var errBuf []byte
	wg.Add(1)
	go func() { defer wg.Done(); errBuf, _ = io.ReadAll(p.Stderr()) }()
	out, _ := io.ReadAll(p.Stdout())
	wg.Wait()
	code, err = p.Wait()
	return out, errBuf, code, err
}

// Dial runs `spritzer x-relay target` in the sprite and returns the exec as a
// net.Conn: writes go to the relay's stdin, reads come from its stdout.
func Dial(ctx context.Context, b Backend, name, target string) (net.Conn, error) {
	p, err := b.Exec(ctx, name, []string{AgentBinary, "x-relay", target}, true)
	if err != nil {
		return nil, err
	}
	go func() { _, _ = io.Copy(io.Discard, p.Stderr()) }()
	return &execConn{p: p, name: name}, nil
}

type execConn struct {
	p    Process
	name string
	once sync.Once
}

func (c *execConn) Read(b []byte) (int, error)  { return c.p.Stdout().Read(b) }
func (c *execConn) Write(b []byte) (int, error) { return c.p.Stdin().Write(b) }
func (c *execConn) CloseWrite() error           { return c.p.Stdin().Close() }
func (c *execConn) Close() error {
	c.once.Do(func() {
		_ = c.p.Stdin().Close()
		_ = c.p.Close()
	})
	return nil
}
func (c *execConn) LocalAddr() net.Addr              { return execAddr("spritzer") }
func (c *execConn) RemoteAddr() net.Addr             { return execAddr(c.name) }
func (c *execConn) SetDeadline(time.Time) error      { return nil }
func (c *execConn) SetReadDeadline(time.Time) error  { return nil }
func (c *execConn) SetWriteDeadline(time.Time) error { return nil }

type execAddr string

func (a execAddr) Network() string { return "exec" }
func (a execAddr) String() string  { return string(a) }

// waitForAgent polls x-ping until the agent answers or ctx ends.
func waitForAgent(ctx context.Context, b Backend, name string) error {
	var last error
	for {
		_, stderr, code, err := Run(ctx, b, name, []string{AgentBinary, "x-ping"}, nil)
		switch {
		case err == nil && code == 0:
			return nil
		case err != nil:
			last = err
		default:
			last = fmt.Errorf("agent not ready (exit %d): %s", code, stderr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sprite %s: agent never answered: %v", name, last)
		case <-time.After(300 * time.Millisecond):
		}
	}
}
