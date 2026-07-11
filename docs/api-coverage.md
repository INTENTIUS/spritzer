# API coverage

spritzer implements the Sprites surface a checkpoint-as-compensation workflow
exercises: create a sprite, run commands in it, checkpoint and restore its
filesystem, and destroy it. A missing or destroyed sprite answers `404` with a
clear JSON error.

## Implemented

| Method | Path | Notes |
| --- | --- | --- |
| POST | `/v1/sprites` | Create a sprite; `name` is required and becomes the id. Returns `{id, url}`. |
| POST | `/v1/sprites/{id}/exec` | Run a command; returns `{stdout, stderr, exitCode}`. |
| POST | `/v1/sprites/{id}/checkpoints` | Deep-copy the filesystem under a label (default `cp-<n>`). Returns `{checkpointId}`. |
| POST | `/v1/sprites/{id}/restore` | Replace the filesystem with a labeled checkpoint; `404` if the label is unknown. |
| DELETE | `/v1/sprites/{id}` | Destroy a sprite. Subsequent operations return `404`. |
| GET | `/v1/sprites/{id}` | Inspect a sprite: `{id, status, url, fs, checkpoints}`. |
| GET | `/_spritzer/health` | Version and coverage report (spritzer-only). |

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

spritzer is wire-compatible with chant's in-process Sprites fake
(`sprites-fake.ts`). The JSON field names — `id`, `url`, `checkpointId`,
`stdout`/`stderr`/`exitCode`, and the `GET` shape's `fs` and `checkpoints` — and
the exec interpreter's behavior match it exactly, so chant's integration suite
passes against the spritzer container image unchanged.
