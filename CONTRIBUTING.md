# Contributing to Tiller Router

Thanks for helping improve Tiller Router. It is a beta, single-service Docker
Compose project. Small, focused changes are easiest to review.

## Before opening a change

1. Check existing issues and any in-repo documentation before opening a change.
   Describe the user-visible behavior, security impact, and compatibility
   implications directly in the change description.
2. Do not include provider credentials, client keys, session cookies, prompts,
   response bodies, or private deployment data in commits, tests, or logs.

## Development and tests

Go is intentionally run in the pinned container, not installed on the host:

    ./tiller-go.sh test ./...
    ./tiller-go.sh vet ./...

For changes to the admin UI, run:

    ./tests/browser/run.sh

Provider-protocol, SDK/CLI compatibility, or restart changes should also run
`./tests/compatibility/run.sh`. Deployment and filesystem-security changes
should run `./tests/runtime-readonly.sh`.

Keep tests deterministic and use local mock upstreams. Run the smallest relevant
set while iterating, then report the exact commands and results in the pull
request. Format Go changes with `./tiller-go.sh fmt ./...`.

## Pull requests

Describe what changed, why, the tests run, and known limitations. Keep unrelated
formatting or generated files out of the patch. New dependencies, services,
ports, persistent files, or changes to the public model/provider identifiers
need explicit maintainer agreement.

By submitting a contribution, you agree that it may be distributed under the
repository's [AGPL-3.0 license](LICENSE).
