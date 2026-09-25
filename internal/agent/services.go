//go:build !windows

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Services are processes the agent owns. They outlive the exec that defined
// them, restart when they crash, and are all started again when the agent
// starts (a container restart is a sprite's cold boot). The model and wire
// shapes follow wisp's guest agent (arugula-salad/wisp internal/agent), which
// follows the Sprites services API.

var (
	// ErrServiceNotFound is a name with no definition.
	ErrServiceNotFound = errors.New("service not found")
	// ErrServiceConflict is a definition that clashes with another one.
	ErrServiceConflict = errors.New("service conflict")

	serviceNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
)

const (
	defaultStopTimeout = 10 * time.Second
	maxRestartBackoff  = 30 * time.Second
	stableRunTime      = 10 * time.Second
)

// ServiceDef is what a caller defines.
type ServiceDef struct {
	Name     string            `json:"name"`
	Cmd      string            `json:"cmd"`
	Args     []string          `json:"args"`
	Needs    []string          `json:"needs"`
	HTTPPort *int              `json:"http_port,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Dir      string            `json:"dir,omitempty"`
}

// ServiceState is the live half.
type ServiceState struct {
	Name         string     `json:"name"`
	Status       string     `json:"status"` // stopped, running, stopping, failed
	PID          int        `json:"pid,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	Error        string     `json:"error,omitempty"`
	RestartCount int        `json:"restart_count,omitempty"`
}

// ServiceWithState is the GET shape: the definition with its state beside it.
type ServiceWithState struct {
	ServiceDef
	State ServiceState `json:"state"`
}

// ServiceEvent is one NDJSON line of a create/start/stop/restart stream.
type ServiceEvent struct {
	Type      string            `json:"type"` // started, stdout, stderr, exit, error, stopping, stopped, complete
	Data      string            `json:"data,omitempty"`
	ExitCode  *int              `json:"exit_code,omitempty"`
	Timestamp int64             `json:"timestamp"`
	LogFiles  map[string]string `json:"log_files,omitempty"`
}

type service struct {
	def         ServiceDef
	state       ServiceState
	cmd         *exec.Cmd
	exited      chan struct{}
	wantRunning bool
	gen         int
	quickFails  int
	logMu       sync.Mutex
	logFile     *os.File
	subs        map[chan ServiceEvent]struct{}
}

// Supervisor owns every service in one sprite.
type Supervisor struct {
	paths Paths
	env   []string

	mu       sync.Mutex
	services map[string]*service
}

// NewSupervisor loads the definitions on disk and starts them all, needs first.
func NewSupervisor(p Paths) *Supervisor {
	sv := &Supervisor{paths: p, services: map[string]*service{}, env: serviceEnv(p)}
	_ = os.MkdirAll(p.ServicesDir(), 0o755)
	_ = os.MkdirAll(p.LogsDir(), 0o755)
	entries, _ := os.ReadDir(p.ServicesDir())
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(p.ServicesDir(), e.Name()))
		if err != nil {
			continue
		}
		var def ServiceDef
		if json.Unmarshal(b, &def) != nil || def.Name == "" {
			continue
		}
		sv.services[def.Name] = newService(def)
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for _, n := range sv.sortedNames() {
		_ = sv.startLocked(n, map[string]bool{})
	}
	return sv
}

// serviceEnv is the environment every service starts from: the agent's own,
// with /.sprite/bin on PATH so a service can call sprite-env.
func serviceEnv(p Paths) []string {
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + p.BinDir() + ":" + strings.TrimPrefix(kv, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+p.BinDir()+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
}

func newService(def ServiceDef) *service {
	return &service{def: def, state: ServiceState{Name: def.Name, Status: "stopped"}, subs: map[chan ServiceEvent]struct{}{}}
}

// LogPath is a service's combined log.
func (sv *Supervisor) LogPath(name string) string {
	return filepath.Join(sv.paths.LogsDir(), name+".log")
}

func (sv *Supervisor) defPath(name string) string {
	return filepath.Join(sv.paths.ServicesDir(), name+".json")
}

func (sv *Supervisor) sortedNames() []string {
	names := make([]string, 0, len(sv.services))
	for n := range sv.services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// List returns every service in name order.
func (sv *Supervisor) List() []ServiceWithState {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	out := []ServiceWithState{}
	for _, n := range sv.sortedNames() {
		s := sv.services[n]
		out = append(out, ServiceWithState{ServiceDef: s.def, State: s.state})
	}
	return out
}

// Get returns one service.
func (sv *Supervisor) Get(name string) (ServiceWithState, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return ServiceWithState{}, ErrServiceNotFound
	}
	return ServiceWithState{ServiceDef: s.def, State: s.state}, nil
}

// Define creates or replaces a definition and writes it to disk. A replaced
// service that is running is stopped; the caller starts it again.
func (sv *Supervisor) Define(def ServiceDef) error {
	if !serviceNameRE.MatchString(def.Name) {
		return fmt.Errorf("invalid service name %q", def.Name)
	}
	if def.Cmd == "" {
		return errors.New("cmd is required")
	}
	if def.Args == nil {
		def.Args = []string{}
	}
	if def.Needs == nil {
		def.Needs = []string{}
	}
	sv.mu.Lock()
	for _, need := range def.Needs {
		if _, ok := sv.services[need]; !ok || need == def.Name {
			sv.mu.Unlock()
			return fmt.Errorf("needs unknown service %q", need)
		}
	}
	if def.HTTPPort != nil {
		for n, o := range sv.services {
			if n != def.Name && o.def.HTTPPort != nil {
				sv.mu.Unlock()
				return fmt.Errorf("%w: service %q already has an HTTP port", ErrServiceConflict, n)
			}
		}
	}
	old, existed := sv.services[def.Name]
	if existed {
		prev := old.def
		old.def = def
		cyclic := sv.hasCycle(def.Name, map[string]bool{})
		old.def = prev
		if cyclic {
			sv.mu.Unlock()
			return errors.New("needs would form a dependency cycle")
		}
	}
	b, _ := json.MarshalIndent(def, "", "  ")
	if err := os.WriteFile(sv.defPath(def.Name), b, 0o644); err != nil {
		sv.mu.Unlock()
		return err
	}
	sv.mu.Unlock()

	if existed {
		if err := sv.Stop(def.Name, defaultStopTimeout); err != nil {
			return err
		}
		sv.mu.Lock()
		old.def = def
		sv.mu.Unlock()
		return nil
	}
	sv.mu.Lock()
	sv.services[def.Name] = newService(def)
	sv.mu.Unlock()
	return nil
}

func (sv *Supervisor) hasCycle(name string, seen map[string]bool) bool {
	if seen[name] {
		return true
	}
	s, ok := sv.services[name]
	if !ok {
		return false
	}
	seen[name] = true
	for _, n := range s.def.Needs {
		if sv.hasCycle(n, seen) {
			return true
		}
	}
	delete(seen, name)
	return false
}

// Start starts a service, and first whatever it needs. Starting a running
// service is a no-op.
func (sv *Supervisor) Start(name string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.startLocked(name, map[string]bool{})
}

func (sv *Supervisor) startLocked(name string, visiting map[string]bool) error {
	s, ok := sv.services[name]
	if !ok {
		return ErrServiceNotFound
	}
	if visiting[name] {
		return errors.New("needs form a dependency cycle")
	}
	visiting[name] = true
	for _, need := range s.def.Needs {
		if err := sv.startLocked(need, visiting); err != nil {
			return fmt.Errorf("start %s (needed by %s): %w", need, name, err)
		}
	}
	s.wantRunning = true
	if s.cmd != nil {
		return nil
	}
	return sv.launchLocked(s)
}

// launchLocked starts the process. Its output goes to the log, one
// "<time> [stream] line" per line, and to any subscriber.
func (sv *Supervisor) launchLocked(s *service) error {
	def := s.def
	cmd := exec.Command(def.Cmd, def.Args...)
	env := append([]string{}, sv.env...)
	for k, v := range def.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Dir = def.Dir
	if cmd.Dir == "" {
		cmd.Dir = homeDir()
	}
	// A process group of its own, so stop reaches whatever it forks.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if s.logFile == nil {
		f, err := os.OpenFile(sv.LogPath(def.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			s.logFile = f
		}
	}
	if err := cmd.Start(); err != nil {
		s.state = ServiceState{Name: def.Name, Status: "failed", Error: err.Error(), RestartCount: s.state.RestartCount}
		sv.emitLocked(s, ServiceEvent{Type: "error", Data: err.Error()})
		return err
	}
	now := time.Now().UTC()
	s.gen++
	gen := s.gen
	s.cmd = cmd
	s.exited = make(chan struct{})
	s.state = ServiceState{Name: def.Name, Status: "running", PID: cmd.Process.Pid, StartedAt: &now, RestartCount: s.state.RestartCount}
	sv.emitLocked(s, ServiceEvent{Type: "started", Data: fmt.Sprintf("%s started (pid %d)", def.Name, cmd.Process.Pid)})

	var pipes sync.WaitGroup
	pipes.Add(2)
	go sv.pump(s, "stdout", stdout, &pipes)
	go sv.pump(s, "stderr", stderr, &pipes)
	go func() {
		pipes.Wait()
		err := cmd.Wait()
		code := exitCode(err)
		sv.mu.Lock()
		defer sv.mu.Unlock()
		close(s.exited)
		if s.gen != gen {
			return
		}
		s.cmd = nil
		sv.emitLocked(s, ServiceEvent{Type: "exit", ExitCode: &code, Data: fmt.Sprintf("%s exited with code %d", def.Name, code)})
		if !s.wantRunning {
			s.state = ServiceState{Name: def.Name, Status: "stopped", RestartCount: s.state.RestartCount}
			sv.emitLocked(s, ServiceEvent{Type: "stopped"})
			return
		}
		// Crashed: restart with backoff, reset once a run has been stable.
		if time.Since(now) >= stableRunTime {
			s.quickFails = 0
		}
		s.quickFails++
		delay := time.Second << min(s.quickFails-1, 5)
		if delay > maxRestartBackoff {
			delay = maxRestartBackoff
		}
		s.state = ServiceState{Name: def.Name, Status: "failed", Error: fmt.Sprintf("exited with code %d; restarting in %s", code, delay), RestartCount: s.state.RestartCount + 1}
		time.AfterFunc(delay, func() {
			sv.mu.Lock()
			defer sv.mu.Unlock()
			if s.gen != gen || !s.wantRunning || s.cmd != nil {
				return
			}
			if _, still := sv.services[def.Name]; still {
				_ = sv.launchLocked(s)
			}
		})
	}()
	return nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

// pump copies one output stream to the log and the subscribers, line by line.
func (sv *Supervisor) pump(s *service, stream string, r interface{ Read([]byte) (int, error) }, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	flush := func(line string) {
		ts := time.Now().UTC()
		s.logMu.Lock()
		if s.logFile != nil {
			_, _ = fmt.Fprintf(s.logFile, "%s [%s] %s\n", ts.Format("2006-01-02T15:04:05.000Z"), stream, line)
		}
		s.logMu.Unlock()
		sv.mu.Lock()
		sv.emitLocked(s, ServiceEvent{Type: stream, Data: line, Timestamp: ts.UnixMilli()})
		sv.mu.Unlock()
	}
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := strings.IndexByte(string(buf), '\n')
			if i < 0 {
				break
			}
			flush(string(buf[:i]))
			buf = buf[i+1:]
		}
		if len(buf) > 64*1024 {
			flush(string(buf))
			buf = buf[:0]
		}
		if err != nil {
			if len(buf) > 0 {
				flush(string(buf))
			}
			return
		}
	}
}

func (sv *Supervisor) emitLocked(s *service, ev ServiceEvent) {
	if ev.Timestamp == 0 {
		ev.Timestamp = time.Now().UnixMilli()
	}
	for ch := range s.subs {
		select {
		case ch <- ev:
		default: // a slow reader misses lines rather than stalling the service
		}
	}
}

// Subscribe streams a service's events until cancel is called.
func (sv *Supervisor) Subscribe(name string) (<-chan ServiceEvent, func(), error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return nil, nil, ErrServiceNotFound
	}
	ch := make(chan ServiceEvent, 256)
	s.subs[ch] = struct{}{}
	return ch, func() {
		sv.mu.Lock()
		delete(s.subs, ch)
		sv.mu.Unlock()
	}, nil
}

// Stop sends SIGTERM to the service's process group and SIGKILL after timeout.
func (sv *Supervisor) Stop(name string, timeout time.Duration) error {
	sv.mu.Lock()
	s, ok := sv.services[name]
	if !ok {
		sv.mu.Unlock()
		return ErrServiceNotFound
	}
	s.wantRunning = false
	cmd, exited := s.cmd, s.exited
	if cmd == nil {
		s.state = ServiceState{Name: name, Status: "stopped", RestartCount: s.state.RestartCount}
		sv.mu.Unlock()
		return nil
	}
	s.state.Status = "stopping"
	sv.emitLocked(s, ServiceEvent{Type: "stopping"})
	pgid := cmd.Process.Pid
	sv.mu.Unlock()

	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(timeout):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-exited
	}
	// Whatever the group left behind goes too.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return nil
}

// Restart stops then starts.
func (sv *Supervisor) Restart(name string, timeout time.Duration) error {
	if err := sv.Stop(name, timeout); err != nil {
		return err
	}
	return sv.Start(name)
}

// Delete stops a service and removes its definition and log.
func (sv *Supervisor) Delete(name string) error {
	sv.mu.Lock()
	for n, o := range sv.services {
		for _, need := range o.def.Needs {
			if need == name && n != name {
				sv.mu.Unlock()
				return fmt.Errorf("%w: %q needs it", ErrServiceConflict, n)
			}
		}
	}
	sv.mu.Unlock()
	if err := sv.Stop(name, defaultStopTimeout); err != nil {
		return err
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return ErrServiceNotFound
	}
	s.logMu.Lock()
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
	s.logMu.Unlock()
	delete(sv.services, name)
	_ = os.Remove(sv.defPath(name))
	_ = os.Remove(sv.LogPath(name))
	return nil
}

// Signal delivers sig to the service's process group.
func (sv *Supervisor) Signal(name string, sig syscall.Signal) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return ErrServiceNotFound
	}
	if s.cmd == nil {
		return fmt.Errorf("%w: %s is not running", ErrServiceConflict, name)
	}
	return syscall.Kill(-s.cmd.Process.Pid, sig)
}

// StopAll stops every service (agent shutdown).
func (sv *Supervisor) StopAll(timeout time.Duration) {
	sv.mu.Lock()
	names := sv.sortedNames()
	sv.mu.Unlock()
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string) { defer wg.Done(); _ = sv.Stop(n, timeout) }(n)
	}
	wg.Wait()
}

// ParseSignal accepts TERM, SIGTERM, 15 and the like.
func ParseSignal(s string) (syscall.Signal, error) {
	up := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "SIG")
	named := map[string]syscall.Signal{
		"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL,
		"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "TERM": syscall.SIGTERM, "CONT": syscall.SIGCONT,
		"STOP": syscall.SIGSTOP,
	}
	if sig, ok := named[up]; ok {
		return sig, nil
	}
	var n int
	if _, err := fmt.Sscanf(up, "%d", &n); err == nil && n > 0 && n < 65 {
		return syscall.Signal(n), nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "/"
}
