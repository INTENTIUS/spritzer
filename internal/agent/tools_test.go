//go:build !windows

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// withStdin replaces os.Stdin for the duration of fn with a pipe fed content,
// restoring the original after.
func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
	go func() {
		_, _ = w.WriteString(content)
		_ = w.Close()
	}()
	fn()
}

// TestFSWriteMode is the acceptance for #29: a file written with an explicit
// mode lands in the sprite with that mode, not the fixed 0644 the write path
// used to always chmod to.
func TestFSWriteMode(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "script.sh")
	withStdin(t, "#!/bin/sh\necho hi\n", func() {
		if code := FS([]string{"write", path, "--mode", "0755"}); code != ExitOK {
			t.Fatalf("write --mode 0755: exit %d", code)
		}
	})
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 0755", st.Mode().Perm())
	}

	// No --mode still defaults to 0644, unchanged from before #29.
	path2 := filepath.Join(dir, "plain.txt")
	withStdin(t, "hello\n", func() {
		if code := FS([]string{"write", path2}); code != ExitOK {
			t.Fatalf("write (no mode): exit %d", code)
		}
	})
	st2, err := os.Stat(path2)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 0644", st2.Mode().Perm())
	}

	// A malformed --mode is a usage error, not a silent fallback.
	path3 := filepath.Join(dir, "bad.txt")
	withStdin(t, "x", func() {
		if code := FS([]string{"write", path3, "--mode", "not-octal"}); code != ExitUsage {
			t.Fatalf("write --mode not-octal: exit %d, want ExitUsage", code)
		}
	})
	if _, err := os.Stat(path3); !os.IsNotExist(err) {
		t.Fatalf("bad.txt should not have been written: %v", err)
	}
}
