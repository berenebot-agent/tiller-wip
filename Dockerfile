# syntax=docker/dockerfile:1.7
FROM golang:1.26.7-alpine AS build
ARG TILLER_VERSION=dev
ARG TILLER_COMMIT=unknown
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    mkdir -p /out/licenses && \
    cp LICENSE /out/licenses/TILLER-LICENSE && \
    cp THIRD_PARTY_NOTICES.md /out/licenses/THIRD_PARTY_NOTICES.md && \
    cp /go/pkg/mod/golang.org/x/crypto@v0.55.0/LICENSE /out/licenses/golang.org-x-crypto-LICENSE && \
    cp /go/pkg/mod/github.com/dustin/go-humanize@v1.0.1/LICENSE /out/licenses/github.com-dustin-go-humanize-LICENSE && \
    cp /go/pkg/mod/github.com/google/uuid@v1.6.0/LICENSE /out/licenses/github.com-google-uuid-LICENSE && \
    cp /go/pkg/mod/github.com/mattn/go-isatty@v0.0.20/LICENSE /out/licenses/github.com-mattn-go-isatty-LICENSE && \
    cp /go/pkg/mod/github.com/ncruces/go-strftime@v0.1.9/LICENSE /out/licenses/github.com-ncruces-go-strftime-LICENSE && \
    cp /go/pkg/mod/github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec/LICENSE /out/licenses/github.com-remyoudompheng-bigfft-LICENSE && \
    cp /go/pkg/mod/golang.org/x/exp@v0.0.0-20250620022241-b7579e27df2b/LICENSE /out/licenses/golang.org-x-exp-LICENSE && \
    cp /go/pkg/mod/golang.org/x/sys@v0.47.0/LICENSE /out/licenses/golang.org-x-sys-LICENSE && \
    cp /go/pkg/mod/modernc.org/sqlite@v1.39.1/LICENSE /out/licenses/modernc.org-sqlite-LICENSE && \
    cp /go/pkg/mod/modernc.org/libc@v1.66.10/LICENSE /out/licenses/modernc.org-libc-LICENSE && \
    cp /go/pkg/mod/modernc.org/libc@v1.66.10/LICENSE-GO /out/licenses/modernc.org-libc-LICENSE-GO && \
    cp /go/pkg/mod/modernc.org/mathutil@v1.7.1/LICENSE /out/licenses/modernc.org-mathutil-LICENSE && \
    cp /go/pkg/mod/modernc.org/mathutil@v1.7.1/mersenne/LICENSE /out/licenses/modernc.org-mathutil-mersenne-LICENSE && \
    cp /go/pkg/mod/modernc.org/memory@v1.11.0/LICENSE /out/licenses/modernc.org-memory-LICENSE && \
    cp /go/pkg/mod/modernc.org/memory@v1.11.0/LICENSE-GO /out/licenses/modernc.org-memory-LICENSE-GO && \
    cp /go/pkg/mod/modernc.org/memory@v1.11.0/LICENSE-LOGO /out/licenses/modernc.org-memory-LICENSE-LOGO && \
    cp /go/pkg/mod/modernc.org/memory@v1.11.0/LICENSE-MMAP-GO /out/licenses/modernc.org-memory-LICENSE-MMAP-GO
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
