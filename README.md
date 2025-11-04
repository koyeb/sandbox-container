# sandbox-container

A secure sandbox container environment for executing commands and managing files. This project provides an HTTP API server that allows you to run shell commands and perform file operations in a controlled, isolated environment.

## Overview

`sandbox-container` is built with Go and packaged as a Docker container based on Ubuntu 22.04. It includes common utilities like `curl`, `wget`, `git`, `python3`, and `jq`, making it suitable for various automation and testing scenarios.

## Features

- **Command Execution**: Run shell commands with custom working directories and environment variables
- **File Operations**: 
  - Write files
  - Read files
  - Delete files and directories
  - Create directories
  - List directory contents
- **Authentication**: All endpoints (except health check) require authentication via bearer token
- **Health Check**: Built-in health check endpoint for monitoring

## Quick Start

### Using Docker

```bash
# Build and run the container
make docker-run

# Or manually with docker
docker build -t koyeb/sandbox .
docker run --rm -p 3030:3030 -p 3031:3031 -e SANDBOX_SECRET=your-secret-here koyeb/sandbox
```

### Building from Source

```bash
# Build the binary
make build

# Run locally
SANDBOX_SECRET=your-secret-here PORT=3030 ./bin/sandbox-executor
```

## Configuration

The server requires the following environment variables:

- `SANDBOX_SECRET` (required): Authentication token for API endpoints
- `PORT` (optional): HTTP server port, defaults to `3030`
- `PROXY_PORT` (optional): TCP proxy server port, defaults to `3031`

## API Endpoints

### Health Check
```
GET /health
```
No authentication required.

### Run Command
```
POST /run
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "cmd": "echo 'Hello World'",
  "cwd": "/tmp",
  "env": {
    "VAR_NAME": "value"
  }
}
```

### Write File
```
POST /write_file
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "path": "/tmp/myfile.txt",
  "content": "file contents"
}
```

### Read File
```
POST /read_file
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "path": "/tmp/myfile.txt"
}
```

### Delete File
```
POST /delete_file
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "path": "/tmp/myfile.txt"
}
```

### Delete Directory
```
POST /delete_dir
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "path": "/tmp/mydir"
}
```

### Make Directory
```
POST /make_dir
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "path": "/tmp/mydir"
}
```

### List Directory
```
POST /list_dir
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "path": "/tmp"
}
```

### Bind Port
```
POST /bind_port
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "port": "8080"
}
```
Configures the TCP proxy (listening on `PROXY_PORT`, default 3031) to forward traffic to the specified port. This allows you to expose services running inside the sandbox to external connections.

**Response on Success:**
```json
{
  "success": true,
  "message": "Port binding configured",
  "port": "8080"
}
```

**Response on Error (Port Already Bound):**
```json
{
  "success": false,
  "error": "Port already bound",
  "current_port": "8080"
}
```
Returns HTTP 409 Conflict if a port is already bound.

### Unbind Port
```
POST /unbind_port
Authorization: Bearer <SANDBOX_SECRET>
```
Removes the TCP proxy port binding for any currently bound port. No request body is required.

**Response:**
```json
{
  "success": true,
  "message": "Port binding removed"
}
```

## Development

### Available Make Commands

- `make build` - Build the binary
- `make clean` - Remove build artifacts
- `make run` - Build and run locally
- `make test` - Run tests
- `make install` - Install binary to GOPATH
- `make docker-build` - Build Docker image for single platform
- `make docker-buildx` - Build Docker image for multiple platforms (amd64, arm64)
- `make docker-push` - Build and push Docker image to registry
- `make docker-run` - Build and run Docker container

### Project Structure

```
.
├── cmd/
│   └── sandbox-executor/    # Main application entry point
├── pkg/
│   └── server/              # HTTP server and handlers
├── Dockerfile               # Multi-stage Docker build
├── Makefile                 # Build and development tasks
└── go.mod                   # Go module definition
```

## Security Considerations

- This container executes arbitrary commands and should only be used in trusted, isolated environments
- Always use strong, randomly generated values for `SANDBOX_SECRET`
- Consider running the container with appropriate resource limits and network isolation
- Do not expose this service directly to the internet without additional security measures

## License

See [LICENSE](LICENSE) file for details.
