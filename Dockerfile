# weaver server (or client) in a container. See docs/CONTAINER.md.
#
#   docker build -t weaver .
#   docker buildx build --platform linux/amd64,linux/arm64 -t weaver .
#
# The Go stage runs on the build machine and cross-compiles, so only the
# small runtime stage needs emulation for a foreign platform.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=0.1.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    case "$TARGETARCH" in \
      arm) export GOARM="${TARGETVARIANT#v}" ;; \
    esac; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/weaver ./cmd/weaver

FROM alpine:3.22
RUN apk add --no-cache iptables iproute2
COPY --from=build /out/weaver /usr/local/bin/weaver
COPY container/entrypoint.sh /usr/local/bin/weaver-entrypoint
RUN chmod 0755 /usr/local/bin/weaver-entrypoint && mkdir -p /var/lib/weaver /etc/weaver
VOLUME /var/lib/weaver
EXPOSE 9328/udp
ENTRYPOINT ["/usr/local/bin/weaver-entrypoint"]
