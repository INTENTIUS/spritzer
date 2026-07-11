package sprite

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/intentius/spritzer/internal/clock"
)

// TestExecInterpreter is a table of command -> (fs mutation, stdout, stderr,
// exit code), porting the cases the fake's fakeExec recognizes.
func TestExecInterpreter(t *testing.T) {
	cases := []struct {
		name     string
		startFS  map[string]string
		cmd      string
		wantFS   map[string]string
		stdout   string
		stderr   string
		exitCode int
	}{
		{
			name:     "echo redirect writes fs, unquoted",
			cmd:      `echo "hello world" > /work/output`,
			wantFS:   map[string]string{"/work/output": "hello world"},
			stdout:   "",
			exitCode: 0,
		},
		{
			name:     "echo redirect single quotes",
			cmd:      `echo 'good' > /state`,
			wantFS:   map[string]string{"/state": "good"},
			exitCode: 0,
		},
		{
			name:     "echo without redirect appends newline to stdout",
			cmd:      "echo hi",
			wantFS:   map[string]string{},
			stdout:   "hi\n",
			exitCode: 0,
		},
		{
			name:     "cat reads fs",
			startFS:  map[string]string{"/state": "good"},
			cmd:      "cat /state",
			wantFS:   map[string]string{"/state": "good"},
			stdout:   "good",
			exitCode: 0,
		},
		{
			name:     "cat missing path yields empty",
			cmd:      "cat /nope",
			wantFS:   map[string]string{},
			stdout:   "",
			exitCode: 0,
		},
		{
			name:     "rm deletes fs key",
			startFS:  map[string]string{"/state": "good"},
			cmd:      "rm /state",
			wantFS:   map[string]string{},
			exitCode: 0,
		},
		{
			name:     "rm -f deletes fs key",
			startFS:  map[string]string{"/state": "good"},
			cmd:      "rm -f /state",
			wantFS:   map[string]string{},
			exitCode: 0,
		},
		{
			name:     "false exits 1",
			cmd:      "false",
			wantFS:   map[string]string{},
			exitCode: 1,
		},
		{
			name:     "true exits 0",
			cmd:      "true",
			wantFS:   map[string]string{},
			exitCode: 0,
		},
		{
			name:     "risky.sh corrupts fs and exits 1",
			cmd:      "./risky.sh",
			wantFS:   map[string]string{"/work/output": "partial-corrupt"},
			stderr:   "risky.sh: failed\n",
			exitCode: 1,
		},
		{
			name:     "unknown command echoes back and exits 0",
			cmd:      "ls -la",
			wantFS:   map[string]string{},
			stdout:   "ls -la\n",
			exitCode: 0,
		},
		{
			name:     "semicolon segments run in order, last exit wins",
			cmd:      "echo bad > /state; false",
			wantFS:   map[string]string{"/state": "bad"},
			exitCode: 1,
		},
		{
			name:     "empty segments are skipped",
			cmd:      " ; echo hi ; ",
			wantFS:   map[string]string{},
			stdout:   "hi\n",
			exitCode: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &Sprite{FS: map[string]string{}}
			for k, v := range tc.startFS {
				sp.FS[k] = v
			}
			got := execInto(sp, tc.cmd)
			if got.Stdout != tc.stdout {
				t.Errorf("stdout = %q, want %q", got.Stdout, tc.stdout)
			}
			if got.Stderr != tc.stderr {
				t.Errorf("stderr = %q, want %q", got.Stderr, tc.stderr)
			}
			if got.ExitCode != tc.exitCode {
				t.Errorf("exitCode = %d, want %d", got.ExitCode, tc.exitCode)
			}
			if !reflect.DeepEqual(sp.FS, tc.wantFS) {
				t.Errorf("fs = %v, want %v", sp.FS, tc.wantFS)
			}
		})
	}
}

// TestCheckpointRestoreRewind is the headline proof: create -> write a key ->
// checkpoint -> run the risky step (corrupts fs, exits 1) -> restore -> the key
// is rewound and status is running again.
func TestCheckpointRestoreRewind(t *testing.T) {
	st := New(clock.NewFake(time.Time{}))
	st.Create("guard-1", "http://localhost/s/guard-1", nil)

	// Seed good state.
	if r, err := st.Exec("guard-1", "echo good > /state"); err != nil || r.ExitCode != 0 {
		t.Fatalf("seed exec = %+v, %v", r, err)
	}
	// Checkpoint the good state.
	label, err := st.Checkpoint("guard-1", "pre-run")
	if err != nil || label != "pre-run" {
		t.Fatalf("checkpoint = %q, %v", label, err)
	}
	// Run the risky step: overwrites /state then fails.
	r, err := st.Exec("guard-1", "echo bad > /state; ./risky.sh")
	if err != nil {
		t.Fatalf("risky exec err = %v", err)
	}
	if r.ExitCode != 1 {
		t.Fatalf("risky exit = %d, want 1", r.ExitCode)
	}
	// The fs is now corrupt.
	view, _ := st.Get("guard-1")
	if view.FS["/state"] != "bad" || view.FS["/work/output"] != "partial-corrupt" {
		t.Fatalf("fs before restore = %v, want corrupt", view.FS)
	}
	// Restore rewinds the fs to the checkpoint (only /state=good) and status runs.
	if err := st.Restore("guard-1", "pre-run"); err != nil {
		t.Fatalf("restore = %v", err)
	}
	view, _ = st.Get("guard-1")
	if !reflect.DeepEqual(view.FS, map[string]string{"/state": "good"}) {
		t.Fatalf("fs after restore = %v, want {/state: good}", view.FS)
	}
	if view.Status != StatusRunning {
		t.Fatalf("status after restore = %q, want running", view.Status)
	}
}

// TestDefaultCheckpointLabel confirms an empty label defaults to cp-<n>.
func TestDefaultCheckpointLabel(t *testing.T) {
	st := New(nil)
	st.Create("s", "http://h/s/s", nil)
	first, _ := st.Checkpoint("s", "")
	second, _ := st.Checkpoint("s", "")
	if first != "cp-1" || second != "cp-2" {
		t.Fatalf("default labels = %q, %q, want cp-1, cp-2", first, second)
	}
}

// TestDestroyedSpriteIsNotFound confirms every op past destroy reports
// ErrNotFound, and a missing sprite does too.
func TestDestroyedSpriteIsNotFound(t *testing.T) {
	st := New(nil)
	st.Create("s", "http://h/s/s", nil)
	if err := st.Destroy("s"); err != nil {
		t.Fatalf("destroy = %v", err)
	}
	if err := st.Destroy("s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second destroy = %v, want ErrNotFound", err)
	}
	if _, err := st.Exec("s", "true"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exec after destroy = %v, want ErrNotFound", err)
	}
	if _, err := st.Get("s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after destroy = %v, want ErrNotFound", err)
	}
	if _, err := st.Checkpoint("s", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("checkpoint after destroy = %v, want ErrNotFound", err)
	}
	if _, err := st.Get("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v, want ErrNotFound", err)
	}
}

// TestRestoreUnknownCheckpoint confirms an unknown label reports
// ErrCheckpointNotFound.
func TestRestoreUnknownCheckpoint(t *testing.T) {
	st := New(nil)
	st.Create("s", "http://h/s/s", nil)
	if err := st.Restore("s", "nope"); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("restore unknown = %v, want ErrCheckpointNotFound", err)
	}
}

// TestCheckpointIsDeepCopied confirms mutating the fs after a checkpoint does not
// change the stored checkpoint (deep copy, not alias).
func TestCheckpointIsDeepCopied(t *testing.T) {
	st := New(nil)
	st.Create("s", "http://h/s/s", nil)
	_, _ = st.Exec("s", "echo one > /f")
	_, _ = st.Checkpoint("s", "cp")
	_, _ = st.Exec("s", "echo two > /f") // mutate after checkpoint
	if err := st.Restore("s", "cp"); err != nil {
		t.Fatalf("restore = %v", err)
	}
	view, _ := st.Get("s")
	if view.FS["/f"] != "one" {
		t.Fatalf("restored /f = %q, want one (checkpoint must be a deep copy)", view.FS["/f"])
	}
}
