# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/intentius/spritzer/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/intentius/spritzer/releases/tag/v0.1.0
