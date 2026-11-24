# The edge worker: zapply-edge plus the headless Chromium it renders pages with.
# Build from the repository root:
#   podman build -f deploy/edge/Dockerfile -t localhost/zapply-edge:0.1.0 .
# (deploy/edge/compose.yaml does this for you.)
FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# The worker is self-contained: nothing else of the module is needed.
COPY cmd/zapply-edge ./cmd/zapply-edge
COPY internal/edgeworker ./internal/edgeworker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/zapply-edge ./cmd/zapply-edge

FROM docker.io/library/debian:12.12-slim
# Chromium comes from Debian's own repository, which patches it for the life of
# the release; rebuild with --pull regularly to pick the patches up.
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium fonts-liberation fonts-noto-color-emoji ca-certificates tini \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --create-home --home-dir /home/edge --shell /usr/sbin/nologin edge
COPY --from=build /out/zapply-edge /usr/local/bin/zapply-edge
# Nothing but /tmp and the home directory (Chromium's profile and caches) is
# written to, so the root filesystem can be read-only and those two tmpfs.
USER 10001:10001
ENV HOME=/home/edge \
    TMPDIR=/tmp \
    ZAPPLY_EDGE_CHROME=/usr/bin/chromium
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/zapply-edge"]
