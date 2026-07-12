package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/intentius/spritzer/internal/sprite"
)

// The control-WebSocket stream ids. Every non-PTY exec frame is a binary
// message whose first byte is the stream id and whose remaining bytes are the
// payload, matching superfly/sprites-go's websocket.go framing.
const (
	streamStdin    byte = 0 // client → server: stdin bytes
	streamStdout   byte = 1 // server → client: stdout bytes
	streamStderr   byte = 2 // server → client: stderr bytes
	streamExit     byte = 3 // server → client: payload[0] is the exit code
	streamStdinEOF byte = 4 // client → server: no more stdin
)

// execSpriteWS upgrades GET /v1/sprites/{id}/exec to a control WebSocket and
// runs the exec interpreter over the framed protocol. The argv is reconstructed
// from the query string: each repeated cmd param is one argv element joined with
// spaces, or a single cmd param is taken as the whole command line; path is the
// argv[0] fallback when no cmd param is present. Non-PTY only: the server writes
// stdout as [streamStdout]<bytes>, stderr as [streamStderr]<bytes>, then a final
// [streamExit]<codeByte> and closes.
func (s *Server) execSpriteWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cmd := reconstructCmd(r)

	// Advertise the control-WebSocket capability on the 101 response so a client
	// can confirm the framed protocol before it starts writing frames.
	w.Header().Set("sprite-capabilities", "control-ws")

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Warn("exec websocket accept failed", "id", id, "err", err)
		return
	}
	// CloseNow is idempotent with a graceful Close; deferring it guards the error
	// paths without double-closing the happy path in any harmful way.
	defer func() { _ = c.CloseNow() }()

	ctx := r.Context()

	// Drain any client stdin frames in the background. The interpreter does not
	// read stdin, so the bytes are discarded, but reading keeps the connection
	// responsive and honors a client that streams stdin then a StreamStdinEOF
	// frame. Draining stops on EOF, read error, or connection close.
	if r.URL.Query().Get("stdin") != "false" {
		go drainStdin(ctx, c)
	}

	result, err := s.store.Exec(id, cmd)
	if err != nil {
		if errors.Is(err, sprite.ErrNotFound) {
			_ = c.Close(websocket.StatusPolicyViolation, "no sprite "+id)
			return
		}
		_ = c.Close(websocket.StatusInternalError, err.Error())
		return
	}

	if result.Stdout != "" {
		if err := writeFrame(ctx, c, streamStdout, []byte(result.Stdout)); err != nil {
			return
		}
	}
	if result.Stderr != "" {
		if err := writeFrame(ctx, c, streamStderr, []byte(result.Stderr)); err != nil {
			return
		}
	}
	if err := writeFrame(ctx, c, streamExit, []byte{byte(result.ExitCode)}); err != nil {
		return
	}

	_ = c.Close(websocket.StatusNormalClosure, "")
}

// reconstructCmd rebuilds the command line from the exec query params. Repeated
// cmd params are argv elements joined with spaces; a single cmd param is the
// whole command line (joining a one-element slice is a no-op). When no cmd param
// is present, path (argv[0]) is the fallback.
func reconstructCmd(r *http.Request) string {
	q := r.URL.Query()
	if cmds := q["cmd"]; len(cmds) > 0 {
		return strings.Join(cmds, " ")
	}
	return q.Get("path")
}

// drainStdin reads and discards client stdin frames until a StreamStdinEOF
// frame, a read error, or context cancellation. It exists so a client that
// speaks the full framing (stdin bytes then EOF) does not stall; the scripted
// interpreter has no stdin to consume.
func drainStdin(ctx context.Context, c *websocket.Conn) {
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case streamStdinEOF:
			return
		case streamStdin:
			// stdin bytes are discarded; the interpreter has no stdin.
		}
	}
}

// writeFrame writes one [streamID]<payload> binary message.
func writeFrame(ctx context.Context, c *websocket.Conn, streamID byte, payload []byte) error {
	frame := make([]byte, 0, len(payload)+1)
	frame = append(frame, streamID)
	frame = append(frame, payload...)
	return c.Write(ctx, websocket.MessageBinary, frame)
}
