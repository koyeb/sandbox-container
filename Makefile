.PHONY: build clean run test docker-build docker-run docker-buildx

BINARY_NAME=sandbox-executor
BUILD_DIR=bin
DOCKER_IMAGE=koyeb/sandbox
PLATFORM?=linux/amd64

build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build -ldflags="-s -w"  -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/sandbox-executor

clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)

run: build
	@echo "Running $(BINARY_NAME)..."
	@./$(BUILD_DIR)/$(BINARY_NAME)

test:
	@echo "Running tests..."
	go test -v ./...

install: build
	@echo "Installing $(BINARY_NAME)..."
	cp $(BUILD_DIR)/$(BINARY_NAME) $(GOPATH)/bin/

docker-build:
	@echo "Building Docker image $(DOCKER_IMAGE) for $(PLATFORM)..."
	docker buildx build --platform $(PLATFORM) -t $(DOCKER_IMAGE) .

docker-buildx:
	@echo "Building Docker image $(DOCKER_IMAGE) for multiple platforms..."
	docker buildx build --platform linux/amd64,linux/arm64 -t $(DOCKER_IMAGE) .

docker-push:
	@echo "Building and pushing Docker image $(DOCKER_IMAGE) for multiple platforms..."
	docker buildx build --platform linux/amd64,linux/arm64 -t $(DOCKER_IMAGE) --push .

docker-run: docker-build
	@echo "Running Docker container..."
	docker run --rm -p 3030:3030 -p 3031:3031 -e SANDBOX_SECRET=test-secret $(DOCKER_IMAGE)

.DEFAULT_GOAL := build
