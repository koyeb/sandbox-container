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
    DENO_INSTALL=/usr/local \
    CARGO_HOME=/root/.cargo \
    RUSTUP_HOME=/root/.rustup \
    BUN_INSTALL=/root/.bun \
    PATH=/usr/local/go/bin:/usr/local/bin:/root/.bun/bin:/root/.cargo/bin:/root/.local/bin:$PATH

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
    ruby-full \
    erlang \
    elixir \
    openjdk-17-jdk-headless \
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
    && curl -fsSL https://bun.sh/install | bash \
    && curl -fsSL https://deno.land/install.sh | sh \
    && curl -fsSL https://mistral.ai/vibe/install.sh | bash \
    && curl -fsSL https://opencode.ai/install | bash \
    && npm install -g @openai/codex \
    && npm install -g @google/gemini-cli \
    # && curl https://cursor.com/install -fsS | bash \
    # && curl -fsSL https://claude.ai/install.sh | bash \
    # && curl -fsSL https://gh.io/copilot-install | bash \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/sandbox-executor /usr/bin/sandbox-executor

COPY LICENSE licenses/LICENSE-* /licenses/


# The entrypoint and mounting latest version of the executor is directly managed by koyeb platform
# to develop on the sandbox-executor, you can uncomment the following line and use koyeb-sdk with your custom image
#ENTRYPOINT ["/usr/bin/sandbox-executor"]

# Licenses
# OpenAI Codex: https://github.com/openai/codex/blob/main/LICENSE (Apache 2.0)
# Google Gemini: https://github.com/google-gemini/gemini-cli/blob/main/LICENSE (Apache 2.0)
# Mistral Vibe: https://github.com/mistralai/mistral-vibe/blob/main/LICENSE (Apache 2.0)
# OpenCode: https://github.com/anomalyco/opencode/blob/dev/LICENSE (MIT)
# Deno: https://github.com/denoland/deno/blob/main/LICENSE.md (MIT)
# Bun: https://github.com/oven-sh/bun/blob/main/LICENSE.md (MIT)
# Elixir: https://github.com/elixir-lang/elixir/blob/main/LICENSE (Apache 2.0)
# Erlang: https://github.com/erlang/otp/blob/master/LICENSE.txt (Apache 2.0)
# JDK: https://github.com/openjdk/jdk/blob/master/LICENSE (GNU GPL v2.0)
# Ruby: https://github.com/ruby/ruby?tab=License-1-ov-file (Ruby License / BSD-2-Clause)

# Anthropic: https://www.anthropic.com/legal/commercial-terms (Commercial Terms of Service)
# Cursor: https://cursor.com/terms-of-service (Commercial Terms of Service)
# GitHub Copilot: https://github.com/github/copilot-cli/blob/main/LICENSE.md
