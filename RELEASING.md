# Releasing

spritzer is released with [`chant release`](https://intentius.io/chant/cli/release/).
A release is one run of the [Release workflow](.github/workflows/release.yml),
started by hand with the release's label. The workflow is generated from the
`release` block in [`chant.config.json`](chant.config.json); regenerate it with
`npx @intentius/chant@0.124.105 release workflow --force` rather than editing it.

The run has three jobs:

1. **gate** picks the newest commit of `main` whose checks passed, at or below
   the dispatched one, and works out the label (`auto` takes it from the
   commits since the previous release).
2. **cut** runs GoReleaser in snapshot mode to build the binaries, archives and
   `checksums.txt`, builds the multi-arch (`linux/amd64`, `linux/arm64`)
   container image from the `Dockerfile`, records the release, and pushes the
   tag `vX.Y.Z`.
3. **publish** pushes the image to `ghcr.io/intentius/spritzer` as `X.Y.Z` and
   `latest` (with `X.Y.Z-amd64` and `X.Y.Z-arm64` for each platform), and
   publishes the GitHub Release with the archives and the checksums. The release notes are the `## [X.Y.Z]`
   section of `CHANGELOG.md`, followed by GitHub's generated notes.

## Steps

1. **Update the changelog.** In `CHANGELOG.md`, move the `## [Unreleased]` items
   into a new `## [X.Y.Z] - YYYY-MM-DD` section and refresh the link references at
   the bottom. Commit it to `main` (via PR). The publish refuses a release whose
   section is missing.

2. **Start the release** from a clean `main`:

   ```sh
   just release X.Y.Z      # or: make release V=X.Y.Z
   ```

   The recipe refuses to proceed unless the working tree is clean, you are on
   `main`, the changelog has a `## [X.Y.Z]` section, and `build`/`vet`/`gofmt`/
   `test` all pass. It then dispatches the Release workflow with the label
   `X.Y.Z`. To see the label chant would pick on its own, run
   `npx @intentius/chant@0.124.105 release next`.

3. **Watch the release run:**

   ```sh
   gh run watch --repo INTENTIUS/spritzer
   ```

   When it's green, confirm the [GitHub Release](https://github.com/INTENTIUS/spritzer/releases)
   has its assets and the image is pullable:

   ```sh
   docker pull ghcr.io/intentius/spritzer:X.Y.Z
   ```

If the publish job fails after the cut, re-run that job from the run's page
(within a day, while the cut's files are kept). The publish skips whatever an
earlier attempt already staged or published.

## First-release-only

A newly created GHCR container package is **private** by default. If
`docker pull` returns `unauthorized`, make the package public once:
**org → Packages → `spritzer` → Package settings → Change visibility → Public**.
This is a one-time setting; later releases inherit it.

## Versioning

spritzer follows [semantic versioning](https://semver.org). While pre-1.0, minor
bumps carry new endpoints / behavior and patch bumps carry fixes. Tags stay
`vX.Y.Z`; the `v*` tags before the move to chant are read as the earlier
releases.
