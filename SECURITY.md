# Security Policy

## Scope

spritzer is a local development and test tool. It is not intended to be exposed
to untrusted networks: it has no authentication, stores everything in memory,
and emulates rather than enforces Sprites' behavior. Run it on `localhost` or
inside a trusted CI network only.

## Supported versions

The latest released `0.x` version receives security fixes. Until a `1.0`
release, older minor versions are not maintained.

## Reporting a vulnerability

Please do not open a public issue for security problems. Instead, use GitHub's
private vulnerability reporting on this repository (Security tab → Report a
vulnerability), or email security@intentius.dev.

Include a description of the issue, steps to reproduce, and the affected
version. We aim to acknowledge reports within three business days and to provide
a remediation timeline after triage.
