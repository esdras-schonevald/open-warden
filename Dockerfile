# Build stage: static open-warden binary
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/open-warden ./cmd/open-warden

# Engine stage: fetch only the native linux-x64 artifact (no Node at runtime)
FROM node:22-bookworm-slim AS engine
ARG ENGINE_VERSION=2.0.29
RUN npm install -g --no-audit --no-fund "@opencode/cli-linux-x64@${ENGINE_VERSION}" \
 && npm cache clean --force

# Runtime stage
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq ripgrep less bash openssh-client \
 && rm -rf /var/lib/apt/lists/*

COPY --from=engine /usr/local/lib/node_modules/@opencode/cli-linux-x64 /opt/engine
COPY --from=build /out/open-warden /usr/local/bin/open-warden
RUN ln -sf /opt/engine/bin/opencode /usr/local/bin/opencode \
 && ln -sf /opt/engine/bin/opencode /usr/local/bin/opencode2

# Single persistent volume: engine credentials/config, state and history.
ENV HOME=/data \
    XDG_CONFIG_HOME=/data/config \
    XDG_DATA_HOME=/data/share \
    XDG_STATE_HOME=/data/state \
    XDG_CACHE_HOME=/data/cache \
    WARDEN_CONFIG=/workspace/warden.yaml

RUN mkdir -p /data /workspace && chmod 0777 /data /workspace
WORKDIR /workspace

ENTRYPOINT ["open-warden"]
CMD ["version"]
