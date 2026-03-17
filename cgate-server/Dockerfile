ARG BUILD_FROM=eclipse-temurin:11-jre

## Stage 1: Build Go web bridge
FROM golang:1.25-alpine AS web-build
WORKDIR /build
COPY web/main.go web/console.html ./
RUN go mod init cgate-web && \
    go get golang.org/x/net/websocket && \
    CGO_ENABLED=0 go build -o cgate-web .

## Stage 2: C-Gate Server runtime
FROM ${BUILD_FROM}

# HA add-on labels
ARG BUILD_ARCH
ARG BUILD_DATE
ARG BUILD_DESCRIPTION
ARG BUILD_NAME
ARG BUILD_REF
ARG BUILD_REPOSITORY
ARG BUILD_VERSION
LABEL \
    io.hass.name="${BUILD_NAME}" \
    io.hass.description="${BUILD_DESCRIPTION}" \
    io.hass.arch="${BUILD_ARCH}" \
    io.hass.type="addon" \
    io.hass.version="${BUILD_VERSION}" \
    maintainer="rbhr" \
    org.opencontainers.image.title="${BUILD_NAME}" \
    org.opencontainers.image.description="${BUILD_DESCRIPTION}" \
    org.opencontainers.image.vendor="rbhr" \
    org.opencontainers.image.authors="rbhr" \
    org.opencontainers.image.source="https://github.com/rbhr/ha-app-C-Gate-Server" \
    org.opencontainers.image.created=${BUILD_DATE} \
    org.opencontainers.image.revision=${BUILD_REF} \
    org.opencontainers.image.version=${BUILD_VERSION}

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
