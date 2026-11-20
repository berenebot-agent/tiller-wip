# Contributing to Tiller Router

Thanks for helping improve Tiller Router. It is a pre-1.0 (`0.x`),
single-service Docker Compose project. Small, focused changes are easiest to review.

## Before opening a change

1. Check existing issues and any in-repo documentation before opening a change.
   Describe the user-visible behavior, security impact, and compatibility
   implications directly in the change description.
2. Do not include provider credentials, client keys, session cookies, prompts,
   response bodies, or private deployment data in commits, tests, or logs.
   The opt-in detailed error logging feature is the sole runtime exception and
   must remain disabled by default in tests.

## Development and tests

Go is intentionally run in the pinned container, not installed on the host:

    ./tiller-go.sh test ./...
    ./tiller-go.sh vet ./...

For changes to the admin UI, run:

    ./tests/browser/run.sh

Provider-protocol, SDK/CLI compatibility, or restart changes should also run
`./tests/compatibility/run.sh`. Deployment and filesystem-security changes
should run `./tests/runtime-readonly.sh`.

Keep tests deterministic and use local mock upstreams. Go tests are in-package
(`internal/<pkg>/<name>_test.go`); browser specs live in `tests/browser/*.spec.js`
and must be wired into the matching lane in `tests/browser/run.sh`. Run the
smallest relevant set while iterating, then report the exact commands and results
in the pull request. Format Go changes with `./tiller-go.sh fmt ./...`. See
`docs/TESTING.md` for the runner and log layout.

## Pull requests

Describe what changed, why, the tests run, and known limitations. Keep unrelated
formatting or generated files out of the patch. New dependencies, services,
ports, persistent files, or changes to the public model/provider identifiers
need explicit maintainer agreement.

## Releases (maintainers)

Releases are cut by pushing a tag to `dellarb/tiller-router`; there is no
manual image build. `.github/workflows/release.yml` runs the Go gate (gofmt,
tests, vet, build, Compose validation), then the container regressions
(browser, compatibility, read-only runtime, clean-install smoke, TLS
reverse-proxy smoke). If all of those pass, it publishes the
`linux/amd64,linux/arm64` image to GHCR and creates the GitHub release.

| Tag | Images published | GitHub release |
|---|---|---|
| `vX.Y.Z` (stable) | `X.Y.Z`, `vX.Y.Z`, `X.Y`, `stable`, `latest` | full release, notes from the `## [X.Y.Z]` section of `CHANGELOG.md` |
| `vX.Y.Z-alpha.N` | `X.Y.Z-alpha.N`, `vX.Y.Z-alpha.N`, `alpha` | prerelease, generated notes |
| `vX.Y.Z-beta.N` / `-rc.N` | the version tags and `beta` | prerelease, generated notes |

Prereleases never move `latest` or `stable`. Any other tag shape fails the
release job before anything is published.

To cut a stable release:

1. On a branch, move the `## [Unreleased]` entries in `CHANGELOG.md` into a new
   `## [X.Y.Z] - YYYY-MM-DD` section, including upgrade notes, and update the
   compare links at the bottom. Merge it to `master` once CI is green.
2. Tag the merged commit and push the tag:

       git tag -a vX.Y.Z -m "Tiller Router vX.Y.Z"
       git push upstream vX.Y.Z

3. Watch the Release workflow. Then check that `ghcr.io/dellarb/tiller-router:X.Y.Z`
   and `:latest` exist and that the startup log reports `version=X.Y.Z`.

If the GitHub release already exists when the workflow reaches that step (for
example, it was drafted by hand), it is left unchanged.

By submitting a contribution, you agree that it may be distributed under the
repository's [AGPL-3.0 license](LICENSE).
