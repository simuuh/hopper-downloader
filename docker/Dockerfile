# syntax=docker/dockerfile:1.7

# Base images are pinned by digest (in addition to the tag, kept for
# readability) so a fresh clone's first build doesn't fail or silently
# drift on a transient registry issue or an upstream tag re-push. Both
# digests are the multi-arch index digest (not a single-platform image
# digest), so TARGETOS/TARGETARCH resolution below still works. Verify/
# update with:
#   docker buildx imagetools inspect node:24-alpine
#   docker buildx imagetools inspect golang:1.26-alpine

# ---- Stage 1: build the web UI ----
FROM node:24-alpine@sha256:44fce696f38ba8c987c4db89954051000bbd05ffd8d8659ba15ed4a8602e937f AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/index.html web/vite.config.ts web/tsconfig*.json web/.oxlintrc.json ./
COPY web/public ./public
COPY web/src ./src
RUN npm run build

# ---- Stage 2: build the Go binaries ----
FROM golang:1.26-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY plugins ./plugins
COPY api ./api
COPY web/embed.go ./web/embed.go
COPY --from=web-builder /src/web/dist ./web/dist

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hopperd ./cmd/hopperd
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hopperctl ./cmd/hopperctl

# ---- Stage 3: minimal runtime image ----
# Not distroless: auto-extract (item 10) shells out to an external unrar
# binary, which distroless's static image (no package manager, no shell)
# can't host. debian:12-slim + apt gets us unrar while keeping the image
# small; ca-certificates is added explicitly since slim doesn't include
# it by default (needed for outbound HTTPS to hosters and
# service.jdownloader.org). hopperd runs as PUID:PGID (default 1000:1000),
# set up by docker/entrypoint.sh — see there.
FROM debian:12-slim

# unrar itself (not the free "unrar-free" reimplementation, which lags on
# RAR5 support) lives in Debian's non-free component, not enabled by
# default — enable it first. Handles both the legacy one-line
# sources.list format and the deb822 debian.sources format the official
# image has used since mid-2023, since which one applies isn't pinned by
# the base image tag.
RUN (test -f /etc/apt/sources.list.d/debian.sources \
        && sed -i 's/^Components: main$/Components: main contrib non-free non-free-firmware/' /etc/apt/sources.list.d/debian.sources) \
    || sed -i 's/ main$/ main contrib non-free non-free-firmware/' /etc/apt/sources.list \
    && apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates unrar \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go-builder /out/hopperd /usr/local/bin/hopperd
COPY --from=go-builder /out/hopperctl /usr/local/bin/hopperctl
COPY --chmod=0755 docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh

ENV HOPPER_CONFIG_FILE=/config/hopper.yaml

VOLUME ["/config", "/downloads"]
# 9667: REST API + Web UI. 9666: Click'n'Load (optional; disable via
# clicknload.enabled: false if not needed).
EXPOSE 9667 9666

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/hopperd", "-healthcheck"]

# No USER: the entrypoint starts as root only to fix /config and /downloads
# ownership for PUID:PGID, then drops to that user before starting hopperd.
# Running the container with --user / compose user: skips that step.
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/usr/local/bin/hopperd"]
