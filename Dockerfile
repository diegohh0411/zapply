# One image for every role: `zapply api`, `zapply migrate`, and later worker/relay.
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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/zapply /zapply
ENV ZAPPLY_ENV=production
EXPOSE 8080
ENTRYPOINT ["/zapply"]
CMD ["api"]
