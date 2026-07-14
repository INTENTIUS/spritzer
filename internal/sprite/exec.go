package sprite

import (
	"regexp"
	"strings"
)

// The recognized command forms. These mirror the interpreter in chant's
// sprites-fake.ts (fakeExec) exactly, so a client cannot tell the two apart.
var (
	reCatRedirect  = regexp.MustCompile(`^cat\s+(\S+)\s*>\s*(\S+)$`)
	reEchoRedirect = regexp.MustCompile(`^echo\s+(.+?)\s*>\s*(\S+)$`)
	reEcho         = regexp.MustCompile(`^echo\s+(.+)$`)
	reCat          = regexp.MustCompile(`^cat\s+(\S+)$`)
	reRm           = regexp.MustCompile(`^rm\s+(?:-f\s+)?(\S+)$`)
)

// execInto runs one command against a sprite's filesystem, mutating sp.FS in
// place, and returns the result. It is not a real shell: it recognizes a small
// set of forms so a test can write a key, then overwrite or fail it, and prove
// that restore rewinds. Segments split on ";" run in order; the exit code is the
// last segment's (shell ";" semantics).
func execInto(sp *Sprite, cmd string) ExecResult {
	var stdout, stderr strings.Builder
	exitCode := 0

	for _, raw := range strings.Split(cmd, ";") {
		seg := strings.TrimSpace(raw)
		if seg == "" {
			continue
		}

		switch {
		case reCatRedirect.MatchString(seg):
			// Copy a file: `cat SRC > DEST`. Lets an Op stage input with a
			// filesystem write, process it with exec, then read the result.
			m := reCatRedirect.FindStringSubmatch(seg)
			sp.FS[m[2]] = sp.FS[m[1]] // missing src -> empty string
			exitCode = 0
		case reEchoRedirect.MatchString(seg):
			m := reEchoRedirect.FindStringSubmatch(seg)
			sp.FS[m[2]] = unquote(m[1])
			exitCode = 0
		case reEcho.MatchString(seg):
			m := reEcho.FindStringSubmatch(seg)
			stdout.WriteString(unquote(m[1]) + "\n")
			exitCode = 0
		case reCat.MatchString(seg):
			m := reCat.FindStringSubmatch(seg)
			stdout.WriteString(sp.FS[m[1]]) // missing path -> empty string
			exitCode = 0
		case reRm.MatchString(seg):
			m := reRm.FindStringSubmatch(seg)
			delete(sp.FS, m[1])
			exitCode = 0
		case seg == "false":
			exitCode = 1
		case seg == "true":
			exitCode = 0
		case seg == "./risky.sh":
			// A scripted failing job: mutates the workspace, then exits non-zero,
			// so a guarded Op can demonstrate checkpoint-as-compensation.
			sp.FS["/work/output"] = "partial-corrupt"
			stderr.WriteString("risky.sh: failed\n")
			exitCode = 1
		default:
			// Unknown command -> echo it back (a no-op success), never real
			// execution.
			stdout.WriteString(seg + "\n")
			exitCode = 0
		}
	}

	return ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}
}

// unquote strips a single pair of matching single or double quotes.
func unquote(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= 2 {
		if (t[0] == '"' && t[len(t)-1] == '"') || (t[0] == '\'' && t[len(t)-1] == '\'') {
			return t[1 : len(t)-1]
		}
	}
	return t
}
