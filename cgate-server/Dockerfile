FROM ghcr.io/home-assistant/base:3.21 AS base

## Stage 1: Build Go web bridge
FROM golang:1.25-alpine AS web-build
WORKDIR /build
COPY web/main.go web/console.html ./
RUN go mod init cgate-web && \
    go get golang.org/x/net/websocket && \
    CGO_ENABLED=0 go build -o cgate-web .

## Stage 2: C-Gate Server runtime
FROM base

# Install Java runtime (HA base is Alpine)
RUN apk add --no-cache openjdk11-jre-headless

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
COPY --from=web-build /build/cgate-web /cgate/cgate-web

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
ENTRYPOINT ["/cgate/run.sh"]
