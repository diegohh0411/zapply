# One image for every role: `zapply api`, `zapply migrate`, and later worker/relay.
FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/zapply ./cmd/zapply

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/zapply /zapply
ENV ZAPPLY_ENV=production
EXPOSE 8080
ENTRYPOINT ["/zapply"]
CMD ["api"]
