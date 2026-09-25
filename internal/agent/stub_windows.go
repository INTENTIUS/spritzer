//go:build windows

package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// The agent runs inside Linux sprites; on Windows every entry point refuses.

var errUnsupported = errors.New("the spritzer agent runs only inside a Linux sprite")

// Run refuses on Windows.
func Run(Paths, *slog.Logger) error { return errUnsupported }

// Install refuses on Windows.
func Install(string) error { return errUnsupported }

func refuse() int { fmt.Fprintln(os.Stderr, errUnsupported); return 64 }

// ExecWrap refuses on Windows.
func ExecWrap(Paths, []string) int { return refuse() }

// Kill refuses on Windows.
func Kill(Paths, string) int { return refuse() }

// Forget refuses on Windows.
func Forget(Paths, string) int { return refuse() }

// Relay refuses on Windows.
func Relay(Paths, string) int { return refuse() }

// FS refuses on Windows.
func FS([]string) int { return refuse() }

// Ping refuses on Windows.
func Ping(Paths) int { return refuse() }

// SpriteEnv refuses on Windows.
func SpriteEnv(Paths, []string) int { return refuse() }
