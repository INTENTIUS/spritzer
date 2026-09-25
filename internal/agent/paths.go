// Package agent is the half of container exec mode that runs inside a sprite.
//
// In container mode every sprite is a container (a Docker container, or a pod
// on Kubernetes) whose main process is `spritzer agent`. The agent supervises
// the sprite's services and serves the in-sprite management socket that
// `sprite-env` talks to, the same surface wisp's guest agent serves on
// /.sprite/api.sock. The spritzer binary is copied into the sprite at
// /.sprite/bin/spritzer (see Install), and the host side reaches the agent only
// by exec'ing that binary: `x-exec` wraps a user command, `x-relay` turns an
// exec into a byte stream to a socket, `x-fs` backs the filesystem API. No
// port inside the sprite has to be reachable from spritzer, which is what lets
// the same code work against Docker Desktop, where container IPs are not
// routable from the host, and against a cluster.
package agent

import (
	"os"
	"path/filepath"
)

// DefaultRoot is where the agent keeps its state inside a sprite, matching
// real Sprites and wisp: definitions in /.sprite/services, logs in
// /.sprite/logs/services, the socket at /.sprite/api.sock.
const DefaultRoot = "/.sprite"

// Paths names every file the agent uses. Root is /.sprite in a sprite; tests
// point it at a temporary directory.
type Paths struct {
	Root string
}

// DefaultPaths honours SPRITZER_AGENT_ROOT (tests), else /.sprite.
func DefaultPaths() Paths {
	if r := os.Getenv("SPRITZER_AGENT_ROOT"); r != "" {
		return Paths{Root: r}
	}
	return Paths{Root: DefaultRoot}
}

// BinDir holds the spritzer binary and the sprite-env link. It is a volume the
// runtime mounts, so it is the same for every image.
func (p Paths) BinDir() string { return filepath.Join(p.Root, "bin") }

// Binary is the spritzer binary inside the sprite.
func (p Paths) Binary() string { return filepath.Join(p.BinDir(), "spritzer") }

// Socket is the management socket sprite-env and the host relay talk to.
func (p Paths) Socket() string { return filepath.Join(p.Root, "api.sock") }

// ServicesDir holds one <name>.json definition per service.
func (p Paths) ServicesDir() string { return filepath.Join(p.Root, "services") }

// LogsDir holds one <name>.log per service.
func (p Paths) LogsDir() string { return filepath.Join(p.Root, "logs", "services") }

// ExecDir holds the pid files of running execs, so the host can kill an exec
// whose client went away.
func (p Paths) ExecDir() string { return filepath.Join(p.Root, "run", "exec") }
