# One image for every role: `zapply api`, `worker`, `relay`, `all` and `migrate`.
# The web app is built first and embedded into the binary.
FROM docker.io/library/node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json web/.npmrc ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist/web/browser/ internal/webui/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/diegohh0411/zapply/internal/webui.BuiltAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o /out/zapply ./cmd/zapply

# Typst CLIs for server-side CV compiles, one per version a published
# template pins (templates/*/*/manifest.json). Static musl builds.
FROM docker.io/library/alpine:3 AS typst
ARG TARGETARCH
RUN set -eu; apk add --no-cache tar xz >/dev/null; \
    case "${TARGETARCH:-amd64}" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) echo "unsupported $TARGETARCH" >&2; exit 1 ;; esac; \
    for v in 0.14.2; do \
      mkdir -p /typst/$v; \
      wget -qO- "https://github.com/typst/typst/releases/download/v$v/typst-$arch-unknown-linux-musl.tar.xz" \
        | tar -xJ -C /typst/$v --strip-components=1 "typst-$arch-unknown-linux-musl/typst"; \
      /typst/$v/typst --version; \
    done

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/zapply /zapply
COPY --from=typst /typst /typst
ENV ZAPPLY_ENV=production TYPST_DIR=/typst
EXPOSE 8080
ENTRYPOINT ["/zapply"]
FROM runtime AS worker
CMD ["worker"]

FROM runtime AS relay
CMD ["relay"]

# api, worker and relay in one process with embedded NATS (single dyno).
FROM runtime AS all
CMD ["all"]

# Keep the API as the default image target.
FROM runtime AS api
CMD ["api"]
