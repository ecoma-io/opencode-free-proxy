# Multi-stage build. Runtime stage is `scratch`: exactly one static binary
# plus the CA bundle needed to verify TLS to the opencode.ai upstream.
# No shell — the Docker HEALTHCHECK works because `healthcheck` is a
# subcommand of the entrypoint binary itself (it GETs the server's own
# /healthz on $OCFP_PORT).
FROM golang:1.26-alpine AS build
WORKDIR /src
ARG VERSION=dev
# Dependency layer before source: it only re-runs when go.mod/go.sum change.
# The module cache (/go/pkg/mod) and compile cache (/root/.cache/go-build)
# are BuildKit cache mounts, not layers — they persist across builds, so a
# source-only edit reuses both the downloaded modules and the already
# compiled packages. Cache-mount contents never enter an image layer, so
# the scratch runtime stage below is unchanged.
COPY go.mod go.sum ./
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/opencode-free-proxy ./cmd/server

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/opencode-free-proxy /app/opencode-free-proxy
USER 65532:65532
EXPOSE 8090
ENV OCFP_PORT=8090
ENTRYPOINT ["/app/opencode-free-proxy"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/app/opencode-free-proxy", "healthcheck"]
