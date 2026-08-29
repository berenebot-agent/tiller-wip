# syntax=docker/dockerfile:1.7
FROM golang:1.26.7-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tiller-router ./cmd/tiller-router
RUN mkdir -p /out/data && chmod 0700 /out/data

FROM scratch
ARG TILLER_UID=65532
ARG TILLER_GID=65532
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=${TILLER_UID}:${TILLER_GID} /out/data /data
COPY --from=build --chown=${TILLER_UID}:${TILLER_GID} /out/tiller-router /tiller-router
USER ${TILLER_UID}:${TILLER_GID}
EXPOSE 8080
ENTRYPOINT ["/tiller-router"]
CMD ["serve"]
