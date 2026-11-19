# syntax=docker/dockerfile:1.7
FROM golang:1.26.7-alpine AS build
ARG TILLER_VERSION=dev
ARG TILLER_COMMIT=unknown
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Collect third-party licenses straight out of the module cache. The module
# versions and license file names are derived from the module graph instead of
# being hardcoded, so bumping a dependency (or a dependency moving/renaming its
# LICENSE) can no longer break the build. `go mod download` is repeated here on
# purpose: this layer re-runs after `COPY . .`, and if the shared module cache
# mount has been pruned in the meantime it is repopulated before anything is
# read from it.
RUN --mount=type=cache,target=/go/pkg/mod set -eu; \
    go mod download; \
    mkdir -p /out/licenses; \
    cp LICENSE /out/licenses/TILLER-LICENSE; \
    cp THIRD_PARTY_NOTICES.md /out/licenses/THIRD_PARTY_NOTICES.md; \
    cp internal/web/assets/D3-LICENSE /out/licenses/d3-LICENSE; \
    main_module=$(go list -m); \
    go list -m -f '{{if .Dir}}{{.Path}}|{{.Dir}}{{end}}' all > /tmp/modules.txt; \
    while IFS='|' read -r module dir; do \
        if [ -z "$module" ] || [ "$module" = "$main_module" ]; then continue; fi; \
        prefix=$(printf '%s' "$module" | tr '/' '-'); \
        find "$dir" -maxdepth 2 -type f \( -name 'LICEN[CS]E*' -o -name 'COPYING*' \) > /tmp/licenses.txt; \
        while IFS= read -r file; do \
            name="$prefix-$(printf '%s' "${file#"$dir"/}" | tr '/' '-')"; \
            name=${name%.md}; \
            name=${name%.txt}; \
            cp "$file" "/out/licenses/$name"; \
        done < /tmp/licenses.txt; \
    done < /tmp/modules.txt
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/tiller-router/tiller-router/internal/version.Version=${TILLER_VERSION} -X github.com/tiller-router/tiller-router/internal/version.Commit=${TILLER_COMMIT}" -o /out/tiller-router ./cmd/tiller-router
RUN mkdir -p /out/data && chmod 0700 /out/data
RUN mkdir -p /out/tmp && chmod 1777 /out/tmp

FROM scratch
# The image defaults to running as root at boot so Tiller's in-process
# privilege drop (internal/privdrop, wired into main) can chown a fresh
# root-owned ./data bind mount and then shed privileges to the runtime user.
# No root-then-drop shell entrypoint is needed — the drop is pure Go and works
# on scratch. Hardened deployments override with `user:` in compose; the image
# also honors the TILLER_UID/TILLER_GID build args for image-level ownership.
ARG TILLER_UID=65532
ARG TILLER_GID=65532
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/licenses /licenses
COPY --from=build --chmod=1777 /out/tmp /tmp
COPY --from=build --chown=${TILLER_UID}:${TILLER_GID} /out/data /data
# The binary is exec-only: root-owned, world read+exec, not writable. This
# still works for the root-at-boot drop because root can exec it; the
# runtime user after the drop does not need to overwrite it.
COPY --from=build --chown=0:0 --chmod=0555 /out/tiller-router /tiller-router
EXPOSE 8080
ENTRYPOINT ["/tiller-router"]
CMD ["serve"]
