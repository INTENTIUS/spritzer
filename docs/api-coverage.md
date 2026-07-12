# API coverage

spritzer implements the Sprites surface a checkpoint-as-compensation workflow
exercises: create a sprite, run commands in it, checkpoint and restore its
filesystem, and destroy it. A missing or destroyed sprite answers `404` with a
clear JSON error.

## Implemented

| Method | Path | Notes |
| --- | --- | --- |
| POST | `/v1/sprites` | Create a sprite; `name` is required and becomes the id. Returns `{id, url}`. |
| GET (WS) | `/v1/sprites/{id}/exec` | Control WebSocket. Reconstructs the command from the query string and streams framed `[streamID][payload]` messages. See [the exec control WebSocket](#the-exec-control-websocket). |
| POST | `/v1/sprites/{id}/checkpoint` | Deep-copy the filesystem under a server-assigned version id (`v1`, `v2`, …). Body is `{comment?}`; streams NDJSON progress ending in `{"event":"complete","id":"v<N>"}`. |
| GET | `/v1/sprites/{id}/checkpoints` | List the checkpoints in creation order as a bare JSON array `[{id, comment, create_time, is_auto}]`. |
| GET | `/v1/sprites/{id}/checkpoints/{cid}` | A single checkpoint's metadata: `{id, comment, create_time, is_auto}`; `404` if the id is unknown. |
| POST | `/v1/sprites/{id}/checkpoints/{cid}/restore` | Replace the filesystem with checkpoint `{cid}` and return the sprite to `running`; streams NDJSON progress. `404` if the id is unknown. |
| DELETE | `/v1/sprites/{id}` | Destroy a sprite. Subsequent operations return `404`. |
| GET | `/v1/sprites/{id}` | Inspect a sprite: `{id, status, url, fs, checkpoints}` (checkpoints as `[{id, comment, create_time, is_auto}]`). |
| GET | `/_spritzer/health` | Version and coverage report (spritzer-only). |

Checkpoints are addressed by a server-assigned version id, not a caller label.
The caller supplies only an optional `comment`; the store assigns `v1`, `v2`, …
in creation order per sprite, stamping a `create_time` and an `is_auto` flag
(false for manual checkpoints). A compensation workflow can therefore use the
`comment` as a stable handle — list the checkpoints and restore the newest one
whose comment matches — while restore itself always takes an explicit id in the
path. Create and restore reply with streaming NDJSON progress
(`application/x-ndjson`): one or more `{"event":"info",...}` lines then a
terminal `{"event":"complete","id":"v<N>"}`.

## The exec control WebSocket

`exec` is a control WebSocket at `GET /v1/sprites/{id}/exec`, matching the real
Sprites SDK (`superfly/sprites-go`, websocket.go). The command is reconstructed
from the query string: each repeated `cmd` param is one argv element (joined with
spaces), or a single `cmd` param is taken as the whole command line; `path`
(argv[0]) is the fallback when no `cmd` is present. `stdin=false` skips stdin
draining.

Non-PTY framing: every WebSocket message is a binary frame whose first byte is a
stream id and whose remaining bytes are the payload.

| Stream | Id | Direction | Payload |
| --- | --- | --- | --- |
| StreamStdin | 0 | client → server | stdin bytes |
| StreamStdout | 1 | server → client | stdout bytes |
| StreamStderr | 2 | server → client | stderr bytes |
| StreamExit | 3 | server → client | one byte: the exit code |
| StreamStdinEOF | 4 | client → server | end of stdin |

The server runs the exec interpreter, writes stdout as `[1]<bytes>`, stderr as
`[2]<bytes>`, then a final `[3]<exitCodeByte>` and closes the connection. The
handshake response advertises `sprite-capabilities: control-ws`. A client with no
stdin passes `stdin=false` (or sends a single `[4]` frame); the interpreter does
not read stdin, so any stdin frames are drained and discarded.

## The exec interpreter

`exec` is not a real shell. A command is split on `;` into segments that run in
order; the exit code is the last segment's. Each segment is matched against a
small set of forms:

| Form | Effect | Exit |
| --- | --- | --- |
| `echo <text> > <path>` | write `fs[path] = unquote(text)` | 0 |
| `echo <text>` | append `unquote(text) + "\n"` to stdout | 0 |
| `cat <path>` | append `fs[path]` (or empty) to stdout | 0 |
| `rm [-f] <path>` | delete `fs[path]` | 0 |
| `false` | (no effect) | 1 |
| `true` | (no effect) | 0 |
| `./risky.sh` | set `fs["/work/output"] = "partial-corrupt"`, append `"risky.sh: failed\n"` to stderr | 1 |
| anything else | append `<segment> + "\n"` to stdout | 0 |

`unquote` strips a single pair of matching single or double quotes.

## Wire fidelity

spritzer mirrors the real Sprites API surface reverse-engineered from
`superfly/sprites-go`: the control-WebSocket exec framing (`[streamID][payload]`
with StreamStdin/Stdout/Stderr/Exit/StdinEOF), the singular NDJSON checkpoint
create, and the bare-array checkpoint list with `create_time` and `is_auto`. The
exec interpreter's behavior still matches chant's in-process Sprites fake
(`sprites-fake.ts`) so a client's checkpoint-as-compensation logic can be
exercised end to end offline.
