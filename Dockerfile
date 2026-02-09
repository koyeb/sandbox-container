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

ENV GO_VERSION=1.22.11 \
    DENO_INSTALL=/usr/local \
    CARGO_HOME=/root/.cargo \
    RUSTUP_HOME=/root/.rustup \
    BUN_INSTALL=/root/.bun \
    PATH=/usr/local/go/bin:/usr/local/bin:/root/.bun/bin:/root/.cargo/bin:$PATH

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN set -eux; apt-get update && apt-get install -y \
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
    ruby-full \
    erlang \
    elixir \
    openjdk-17-jre-headless \
    && curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get update \
    && apt-get install -y nodejs \
    && curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz | tar -C /usr/local -xzf - \
    && curl -fsSL https://sh.rustup.rs | bash -s -- -y --profile minimal \
    && curl -fsSL https://bun.sh/install | bash \
    && curl -fsSL https://deno.land/install.sh | sh \
    && curl -fsSL https://gh.io/copilot-install | bash \
    && curl -fsSL https://mistral.ai/vibe/install.sh | bash \
    && curl -fsSL https://claude.ai/install.sh | bash \
    && curl -fsSL https://opencode.ai/install | bash \
    && curl https://cursor.com/install -fsS | bash \
    && npm install -g @openai/codex \
    && npm install -g @google/gemini-cli \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/sandbox-executor /usr/bin/sandbox-executor


# The entrypoint and mounting latest version of the executor is directly managed by koyeb platform
# to develop on the sandbox-executor, you can uncomment the following line and use koyeb-sdk with your custom image
#ENTRYPOINT ["/usr/bin/sandbox-executor"]
