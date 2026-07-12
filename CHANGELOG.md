# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-07-11

Aligns exec and checkpoints with the real Sprites API surface reverse-engineered
from `superfly/sprites-go` (websocket.go, checkpoint.go). Exec is now a control
WebSocket speaking the framed stream protocol, and checkpoint create/restore
stream NDJSON progress.

### Changed

- `exec` is now a control WebSocket at `GET /v1/sprites/{name}/exec`, replacing
  the JSON `POST` endpoint. The command is reconstructed from the query string
  (`cmd` repeated per argv element, or a single `cmd` as the whole command line;
  `path` is the argv[0] fallback). Every message is a binary frame
  `[streamID][payload]`: StreamStdin=0, StreamStdout=1, StreamStderr=2,
  StreamExit=3 (payload[0] is the exit code), StreamStdinEOF=4. The server writes
  stdout as `[1]<bytes>`, stderr as `[2]<bytes>`, then `[3]<exitCodeByte>` and
  closes, matching the real Sprites SDK's non-PTY framing. The handshake response
  advertises `sprite-capabilities: control-ws`.
- Checkpoint create moved to the singular `POST /v1/sprites/{name}/checkpoint`
  and now streams line-delimited NDJSON progress events (`application/x-ndjson`):
  an `info` event then a terminal `{"event":"complete","id":"v<N>"}` carrying the
  server-assigned version id. The old plural create and its `{id}` JSON body are
  removed.
- `GET /v1/sprites/{name}/checkpoints` now returns a bare JSON array
  `[{id, comment, create_time, is_auto}]` (creation order), not a
  `{checkpoints: [...]}` wrapper. Each checkpoint gains a `create_time` timestamp
  and an `is_auto` flag (false for manual checkpoints).
- Restore (`POST /v1/sprites/{name}/checkpoints/{id}/restore`) now streams NDJSON
  progress events; an unknown sprite or checkpoint id is still a `404` before the
  stream starts.
- `GET /v1/sprites/{name}`'s `checkpoints` projection carries the richer
  `{id, comment, create_time, is_auto}` shape.
- `/_spritzer/health`'s implemented-path list reflects the new surface (WS exec,
  singular checkpoint create, individual checkpoint GET, checkpoints list, and
  restore).

### Added

- `GET /v1/sprites/{name}/checkpoints/{id}` returns a single checkpoint's
  metadata (`{id, comment, create_time, is_auto}`); an unknown id is a `404`.
- `github.com/coder/websocket` as the WebSocket dependency for the control-exec
  endpoint.

## [0.2.0] - 2026-07-11

Corrects the checkpoint/restore surface to the confirmed real Sprites API. The
provisional v0.1.0 shape treated the caller's label as the checkpoint id; the
real API assigns the id and the caller controls only a comment.

### Changed

- Checkpoints are now addressed by a server-assigned version id (`v1`, `v2`, …),
  assigned sequentially per sprite in creation order, not by a caller label. The
  create body is `{comment?}` (an optional string) and the response is `{id}`
  (the server id), replacing the previous `{label}` body and `{checkpointId}`
  response.
- Restore moved to `POST /v1/sprites/{name}/checkpoints/{id}/restore`, taking the
  checkpoint id in the path with an empty body; an unknown id is a `404`. The old
  `POST /v1/sprites/{name}/restore` route (with a `{checkpoint}` body) is removed.
- `GET /v1/sprites/{name}` now exposes `checkpoints` as `[{id, comment}]` instead
  of a sorted list of labels.
- `/_spritzer/health`'s implemented-path list reflects the new surface (drops the
  top-level `.../restore`, adds `.../checkpoints/{id}/restore` and
  `GET .../checkpoints`).

### Added

- `GET /v1/sprites/{name}/checkpoints` lists a sprite's checkpoints as
  `{checkpoints: [{id, comment}]}` in creation order, so a compensation workflow
  can pick the newest checkpoint whose comment matches a stable handle.

### Note

- The REST `exec` response shape (`{stdout, stderr, exitCode}`) is kept unchanged
  but is provisional: real exec is WebSocket-primary and the REST response shape
  is not published (`TODO(confirm)`).

## [0.1.0] - 2026-07-11

### Added

- Stateful in-memory store of sprites keyed by name, each with a filesystem
  (path → contents) and a set of labeled checkpoints, guarded by a mutex and
  wired to an injected clock.
- `exec` interpreter matching chant's in-process Sprites fake byte for byte:
  segments split on `;` run in order (last exit code wins), recognizing
  `echo <text> > <path>`, `echo <text>`, `cat <path>`, `rm [-f] <path>`,
  `true`/`false`, `./risky.sh`, and an echo-back default.
- Checkpoint / restore semantics: a checkpoint deep-copies the filesystem under a
  label (defaulting to `cp-<n>`); a restore replaces the filesystem with that
  copy and returns the sprite to `running`. An unknown label is a `404`.
- HTTP server covering `POST /v1/sprites`, `POST /v1/sprites/{id}/exec`,
  `POST /v1/sprites/{id}/checkpoints`, `POST /v1/sprites/{id}/restore`,
  `DELETE /v1/sprites/{id}`, and `GET /v1/sprites/{id}`, using the standard
  library router. A destroyed or missing sprite returns `404` on any operation.
- `/_spritzer/health` endpoint reporting version and implemented paths.
- `spritzer` command with `-addr` / `SPRITZER_ADDR` configuration, structured
  logging, and graceful shutdown.
- Distroless container image, GoReleaser configuration, mkdocs-material doc site,
  and CI.

[Unreleased]: https://github.com/intentius/spritzer/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/intentius/spritzer/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/intentius/spritzer/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/intentius/spritzer/releases/tag/v0.1.0
