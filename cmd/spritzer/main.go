// Command spritzer runs a standalone, stateful local emulator of the Fly.io
// Sprites API. Point a Sprites client at it by setting SPRITES_BASE_URL to the
// address it listens on (default http://localhost:4290).
//
// The same binary is the in-sprite agent in container exec mode: `spritzer
// agent` is a sprite's main process, `sprite-env` (a link to this binary) is
// the in-sprite CLI, and the x-* subcommands are what the server execs inside
// a sprite. See internal/agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/intentius/spritzer/internal/agent"
	"github.com/intentius/spritzer/internal/runtime"
	"github.com/intentius/spritzer/internal/server"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// defaultAddr is spritzer's default listen address.
const defaultAddr = ":4290"

func main() {
	if code, ok := subcommand(os.Args); ok {
		os.Exit(code)
	}
	if err := run(); err != nil {
		slog.Error("spritzer exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("SPRITZER_ADDR", defaultAddr), "address to listen on (host:port)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *showVersion {
		logger.Info("spritzer", "version", version)
		return nil
	}

	opts := server.Options{Version: version, Logger: logger}
	mode := envOr("SPRITZER_EXEC", "interpreter")
	switch mode {
	case "interpreter":
	case "container":
		rt, err := newRuntime(logger)
		if err != nil {
			return err
		}
		opts.Runtime = rt
		opts.URLDomain = os.Getenv("SPRITZER_URL_DOMAIN")
	default:
		return fmt.Errorf("SPRITZER_EXEC=%q: want interpreter (the default) or container", mode)
	}
	srv := server.New(opts)
	if opts.Runtime != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := srv.Adopt(ctx); err != nil {
			logger.Warn("could not list existing sprites", "err", err)
		}
		cancel()
	}

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("spritzer listening",
			"addr", *addr,
			"version", version,
			"exec", mode,
			"hint", "set SPRITES_BASE_URL=http://localhost"+portHint(*addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	}

	cancelBase()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("spritzer stopped cleanly")
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// portHint renders the ":4290" style suffix for the SPRITES_BASE_URL hint.
func portHint(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return addr
	}
	if i := lastColon(addr); i >= 0 {
		return addr[i:]
	}
	return defaultAddr
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

// subcommand runs the in-sprite entry points: `sprite-env` by name, and the
// agent's subcommands. ok is false for the server itself.
func subcommand(args []string) (code int, ok bool) {
	p := agent.DefaultPaths()
	if filepath.Base(args[0]) == "sprite-env" {
		return agent.SpriteEnv(p, args[1:]), true
	}
	if len(args) < 2 {
		return 0, false
	}
	rest := args[2:]
	switch args[1] {
	case "agent":
		if err := agent.Run(p, nil); err != nil {
			fmt.Fprintln(os.Stderr, "spritzer agent:", err)
			return 1, true
		}
		return 0, true
	case "agent-install":
		dir := p.BinDir()
		if len(rest) > 0 {
			dir = rest[0]
		}
		if err := agent.Install(dir); err != nil {
			fmt.Fprintln(os.Stderr, "spritzer agent-install:", err)
			return 1, true
		}
		return 0, true
	case "sprite-env":
		return agent.SpriteEnv(p, rest), true
	case "x-exec":
		return agent.ExecWrap(p, rest), true
	case "x-kill", "x-forget", "x-relay":
		if len(rest) != 1 {
			return agent.ExitUsage, true
		}
		switch args[1] {
		case "x-kill":
			return agent.Kill(p, rest[0]), true
		case "x-forget":
			return agent.Forget(p, rest[0]), true
		}
		return agent.Relay(p, rest[0]), true
	case "x-fs":
		return agent.FS(rest), true
	case "x-ping":
		return agent.Ping(p), true
	}
	return 0, false
}

// newRuntime picks the container runtime for SPRITZER_EXEC=container:
// SPRITZER_RUNTIME=docker|kubernetes, defaulting to kubernetes inside a
// cluster and docker elsewhere.
func newRuntime(logger *slog.Logger) (runtime.Backend, error) {
	cfg := runtime.Config{
		SpriteImage:   envOr("SPRITZER_SPRITE_IMAGE", "node:22-bookworm"),
		AgentImage:    os.Getenv("SPRITZER_AGENT_IMAGE"),
		CreateTimeout: 5 * time.Minute,
	}
	if v := os.Getenv("SPRITZER_CREATE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("SPRITZER_CREATE_TIMEOUT: %w", err)
		}
		cfg.CreateTimeout = d
	}
	if cfg.AgentImage == "" {
		if version == "dev" || strings.Contains(version, "-") {
			return nil, errors.New("SPRITZER_AGENT_IMAGE is required for a development build: " +
				"the image that carries this build's Linux binary (for example one built with `just docker`)")
		}
		cfg.AgentImage = "ghcr.io/intentius/spritzer:" + strings.TrimPrefix(version, "v")
	}
	kind := os.Getenv("SPRITZER_RUNTIME")
	if kind == "" {
		kind = "docker"
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			kind = "kubernetes"
		}
	}
	var rt runtime.Backend
	var err error
	switch kind {
	case "docker":
		rt, err = runtime.NewDocker(cfg)
	case "kubernetes", "k8s":
		rt, err = runtime.NewKubernetes(cfg, os.Getenv("SPRITZER_NAMESPACE"))
	default:
		return nil, fmt.Errorf("SPRITZER_RUNTIME=%q: want docker or kubernetes", kind)
	}
	if err != nil {
		return nil, err
	}
	logger.Info("container exec mode", "runtime", rt.Kind(), "sprite_image", cfg.SpriteImage, "agent_image", cfg.AgentImage)
	return rt, nil
}
