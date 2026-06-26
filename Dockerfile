# build executor
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w -X main.Version=${VERSION}" -o sandbox-executor ./cmd/sandbox-executor


# run stage
FROM ubuntu:22.04

ENV GO_VERSION=1.25.0 \
    CARGO_HOME=/root/.cargo \
    RUSTUP_HOME=/root/.rustup \
    PATH=/usr/local/go/bin:/usr/local/bin:/root/.cargo/bin:/root/.local/bin:$PATH

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN set -eux; apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    wget \
    zip \
    unzip \
    git \
    file \
    procps \
    python3 \
    python3-pip \
    jq \
    gnupg \
    zstd \
    bash \
    build-essential \
    xz-utils \
    tar \
    && curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get update \
    && apt-get install -y --no-install-recommends nodejs \
    && GO_ARCH="$(dpkg --print-architecture)" \
    && case "$GO_ARCH" in \
         amd64) GO_ARCH=amd64 ;; \
         arm64) GO_ARCH=arm64 ;; \
         *) echo "Unsupported architecture for Go: $GO_ARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" | tar -C /usr/local -xzf - \
    && curl -fsSL https://sh.rustup.rs | bash -s -- -y --profile minimal \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/sandbox-executor /usr/bin/sandbox-executor

COPY LICENSE licenses/LICENSE-* /licenses/


# The entrypoint and mounting latest version of the executor is directly managed by koyeb platform
# to develop on the sandbox-executor, you can uncomment the following line and use koyeb-sdk with your custom image
#ENTRYPOINT ["/usr/bin/sandbox-executor"]
