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
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/zapply ./cmd/zapply

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/zapply /zapply
ENV ZAPPLY_ENV=production
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
