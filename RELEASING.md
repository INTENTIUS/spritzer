# Releasing

spritzer is released with [`chant release`](https://intentius.io/chant/cli/release/).
A release is one run of the [Release workflow](.github/workflows/publish.yml),
started by hand with a label. The workflow is generated from the `release`
block in [`chant.config.json`](chant.config.json); regenerate it with
`npx @intentius/chant@0.124.107 release workflow --forge github` rather than
editing it.

## Starting a release

```sh
gh workflow run publish.yml -f label=auto     # chant picks the version from the commits
gh workflow run publish.yml -f label=0.6.5    # or give a bare version
```

`just release` and `just release 0.6.5` run the same dispatch. To see the label
chant would pick on its own, run `npx @intentius/chant@0.124.107 release next`.

Follow the run with `gh run watch --repo INTENTIUS/spritzer`. When it is green,
check that the [GitHub Release](https://github.com/INTENTIUS/spritzer/releases)
has its assets and that the image pulls:

```sh
docker pull ghcr.io/intentius/spritzer:X.Y.Z
```

## What the run does

1. **gate** picks the newest commit of `main` whose checks passed, at or below
   the dispatched one, and works out the label (`auto` takes it from the
   commits since the previous release).
2. **cut** runs GoReleaser in snapshot mode to build the binaries for linux,
   darwin and windows on amd64 and arm64, the archives (`tar.gz`, `zip` on
   windows) and `checksums.txt`. It builds the `linux/amd64` and `linux/arm64`
   container image from the `Dockerfile`, records the release, and pushes the
   tag `vX.Y.Z`.
3. **publish** pushes the image to `ghcr.io/intentius/spritzer` as `X.Y.Z` and
   `latest`, with `X.Y.Z-amd64` and `X.Y.Z-arm64` for each platform. It also
   publishes the GitHub Release with the archives and `checksums.txt`.

The release notes are generated from the commits since the previous release,
leaving out `docs`, `test` and `chore` commits. `CHANGELOG.md` is not read, so
nothing needs writing before a release; use conventional commit messages so the
notes read well.

If the publish job fails after the cut, re-run that job from the run's page
(within a day, while the cut's files are kept). The publish skips whatever an
earlier attempt already staged or published.

## First release of the image

A newly created GHCR container package is private by default. If
`docker pull` returns `unauthorized`, make the package public once:
org, Packages, `spritzer`, Package settings, Change visibility, Public. Later
releases keep that setting.

## Versioning

spritzer follows [semantic versioning](https://semver.org). While pre-1.0, minor
bumps carry new endpoints and behavior, and patch bumps carry fixes. Tags are
`vX.Y.Z`; the `v*` tags from before the move to chant are read as earlier
releases.
