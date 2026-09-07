FROM ghcr.io/home-assistant/base:3.21@sha256:6f40fd4ef842d88b51bbd69d00ac4f7ba6a8ac4b23db87c3b529ea970158c731 AS base

FROM base AS runtime-base
# Pin added APKs as well as the base; unavailable versions fail the build.
RUN apk add --no-cache \
    java-common=0.5-r0 libffi=3.4.7-r0 libtasn1=4.21.0-r0 \
    p11-kit=0.26.2-r0 p11-kit-trust=0.26.2-r0 java-cacerts=1.1-r0 \
    openjdk11-jre-headless=11.0.30_p7-r0 sqlite=3.48.0-r4

## Stage 1: Build Go web bridge
# Go 1.25.14; keep the CI race-test toolchain in checks.yaml in sync.
FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS web-build
WORKDIR /build
COPY web/main.go web/storage.go web/console.html ./
RUN go mod init cgate-web && \
    go get golang.org/x/net/websocket@v0.58.0 && \
    CGO_ENABLED=0 go build -o cgate-web .

# Every runtime image depends on tests of its own compiled inputs and SQLite.
FROM runtime-base AS web-test
COPY --from=web-build /usr/local/go /usr/local/go
COPY --from=web-build /go/pkg/mod /go/pkg/mod
COPY --from=web-build /build /build
ENV PATH="/usr/local/go/bin:${PATH}" GOPATH=/go CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /build
COPY web/*_test.go ./
COPY web/testdata/ ./testdata/
RUN test -z "$(gofmt -l .)" && go vet ./... && go test ./...

## Stage 2: C-Gate Server runtime
FROM runtime-base

# HA add-on labels
# BUILD_ARCH and BUILD_VERSION are supplied by the builder build-image action,
# which also overrides io.hass.arch/io.hass.version and the OCI created/source/
# version labels at build time. The defaults below only apply to local builds.
ARG BUILD_ARCH="amd64"
ARG BUILD_VERSION="dev"
LABEL \
    io.hass.name="C-Gate Server" \
    io.hass.description="Schneider Electric SpaceLogic C-Gate Server for C-Bus home automation" \
    io.hass.arch="${BUILD_ARCH}" \
    io.hass.type="addon" \
    io.hass.version="${BUILD_VERSION}" \
    io.hass.url="https://github.com/rbhr/ha-app-C-Gate-Server" \
    maintainer="rbhr" \
    org.opencontainers.image.title="C-Gate Server" \
    org.opencontainers.image.description="Schneider Electric SpaceLogic C-Gate Server for C-Bus home automation" \
    org.opencontainers.image.vendor="rbhr" \
    org.opencontainers.image.authors="rbhr" \
    org.opencontainers.image.licenses="MIT" \
    org.opencontainers.image.source="https://github.com/rbhr/ha-app-C-Gate-Server" \
    org.opencontainers.image.version="${BUILD_VERSION}"

# Create directories
RUN mkdir -p /cgate/tag /cgate/config /cgate/logs /cgate/defaults /cgate/tag-defaults

# Copy C-Gate distribution
COPY cgate-dist/ /cgate/

# Copy Go web bridge from build stage
COPY --from=web-test /build/cgate-web /cgate/cgate-web

# Copy default configuration files
COPY config/access.txt /cgate/defaults/access.txt
COPY config/C-groups.txt /cgate/defaults/C-groups.txt
COPY config/logback.xml /cgate/defaults/logback.xml

# Copy default tag databases
COPY tag/ /cgate/tag-defaults/

# Copy startup script
COPY run.sh /cgate/run.sh
RUN chmod +x /cgate/run.sh /cgate/cgate-web /cgate/cgate.sh && \
    chmod +x /cgate/yajsw/bin/*.sh

WORKDIR /cgate
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD curl --fail --silent --max-time 3 http://127.0.0.1:8980/health >/dev/null || exit 1
ENTRYPOINT ["/cgate/run.sh"]
