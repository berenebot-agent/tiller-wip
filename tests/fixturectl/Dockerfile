# syntax=docker/dockerfile:1.7
# Builds the fixturectl test utility. The build context is the repo ROOT (run.sh
# invokes `docker build -f tests/fixturectl/Dockerfile .`), reusing the root
# .dockerignore so node_modules, data/, and .git never enter the context.
#
# The final stage is scratch exporting a single static binary; run.sh extracts
# it with `docker create` + `docker cp` and mounts it read-only into the browser
# test container. It is never part of the router image.
FROM golang:1.26.7-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fixturectl ./tests/fixturectl

FROM scratch AS export
COPY --from=build /out/fixturectl /fixturectl
