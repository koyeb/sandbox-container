# build executor
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o sandbox-executor ./cmd/sandbox-executor


# run stage
FROM ubuntu:22.04

RUN apt-get update && apt-get install -y \
    curl \
    wget \
    zip \
    unzip \
    git \
    python3 \
    python3-pip \
    jq \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/sandbox-executor /usr/bin/sandbox-executor


ENTRYPOINT ["/usr/bin/sandbox-executor"]
