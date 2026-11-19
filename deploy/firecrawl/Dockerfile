# Firecrawl Local (https://docs.firecrawl.dev/quickstarts/firecrawl-local):
# one binary serving the Firecrawl v2 scrape API, used to read job postings.
# Firecrawl publishes no image, so it is built from crates.io at a pinned version.
FROM docker.io/library/rust:1-bookworm AS build
ARG FIRECRAWL_LOCAL_VERSION=0.1.0
RUN cargo install firecrawl-local --locked --version "${FIRECRAWL_LOCAL_VERSION}" --root /out

# Chromium renders pages that need JavaScript, with its sandbox on (see
# deploy/compose.yaml).
FROM docker.io/library/debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates chromium fonts-liberation \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 10001 firecrawl
COPY --from=build /out/bin/firecrawl-local /usr/local/bin/firecrawl-local
USER firecrawl
ENV FIRECRAWL_CHROME_PATH=/usr/bin/chromium \
    FIRECRAWL_LOCAL_HOME=/home/firecrawl/.firecrawl-local
EXPOSE 3002
ENTRYPOINT ["firecrawl-local"]
CMD ["serve", "--host", "0.0.0.0"]
