# Releasing

spritzer releases are tag-driven: pushing a `vX.Y.Z` tag triggers the
[Release workflow](.github/workflows/release.yml), which runs GoReleaser to build
the binaries, the checksums, and the multi-arch container image, publishes a
GitHub Release, and pushes the image to `ghcr.io/intentius/spritzer`.

## Steps

1. **Update the changelog.** In `CHANGELOG.md`, move the `## [Unreleased]` items
   into a new `## [X.Y.Z] - YYYY-MM-DD` section and refresh the link references at
   the bottom. Commit it to `main` (via PR).

2. **Cut the release** from a clean `main`:

   ```sh
   just release X.Y.Z      # or: make release V=X.Y.Z
   ```

   The recipe refuses to proceed unless the working tree is clean, you are on
   `main`, the changelog has a `## [X.Y.Z]` section, and `build`/`vet`/`gofmt`/
   `test` all pass. It then tags `vX.Y.Z` and pushes the tag.

3. **Watch the release build:**

   ```sh
   gh run watch --repo INTENTIUS/spritzer
   ```

   When it's green, confirm the [GitHub Release](https://github.com/INTENTIUS/spritzer/releases)
   has its assets and the image is pullable:

   ```sh
   docker pull ghcr.io/intentius/spritzer:X.Y.Z
   ```

## First-release-only

A newly created GHCR container package is **private** by default. If
`docker pull` returns `unauthorized`, make the package public once:
**org → Packages → `spritzer` → Package settings → Change visibility → Public**.
This is a one-time setting; later releases inherit it.

## Versioning

spritzer follows [semantic versioning](https://semver.org). While pre-1.0, minor
bumps carry new endpoints / behavior and patch bumps carry fixes.
