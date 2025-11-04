# sandbox-container

A secure sandbox container environment for executing commands and managing files. This project provides an HTTP API server that allows you to run shell commands and perform file operations in a controlled, isolated environment.

## Overview

`sandbox-container` is built with Go and packaged as a Docker container based on Ubuntu 22.04. It includes common utilities like `curl`, `wget`, `git`, `python3`, and `jq`, making it suitable for various automation and testing scenarios.

## Features

- **Command Execution**: Run shell commands with custom working directories and environment variables
- **Streaming Output**: Stream command output in real-time using Server-Sent Events
- **Background Process Management**: Start, monitor, and control long-running background processes with real-time log streaming
- **File Operations**: 
  - Write files
  - Read files
  - Delete files and directories
  - Create directories
  - List directory contents
- **Authentication**: All endpoints (except health check) require authentication via bearer token
- **Health Check**: Built-in health check endpoint for monitoring
- **Port Binding**: Expose internal services via TCP proxy

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

### Run Command (Streaming)
```
POST /run_streaming
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
Executes a shell command and streams the output in real-time using Server-Sent Events (SSE). The response includes:

- `output` events for each line of stdout/stderr as it's produced
- `complete` event when the command finishes with exit code
- `error` event if the command fails to start

**Example Response Stream:**
```
event: output
data: {"stream":"stdout","data":"line 1"}

event: output
data: {"stream":"stderr","data":"warning message"}

event: complete
data: {"code":0,"error":false}
```

Use this endpoint for long-running commands where you want real-time output. For simple commands where buffered output is acceptable, use `/run` instead.

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

## Background Process Management

The sandbox executor can manage long-running background processes with real-time log streaming.

### Start Process
```
POST /start_process
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "cmd": "python -u app.py",
  "cwd": "/path/to/working/directory",
  "env": {
    "PORT": "8080",
    "DEBUG": "true"
  }
}
```
Starts a new background process. Returns a unique process ID, PID, and status.

**Response (201 Created):**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "pid": 12345,
  "status": "running"
}
```

**Process Status Values:**
- `running`: Process is currently executing
- `completed`: Process exited successfully (exit code 0)
- `failed`: Process exited with non-zero exit code
- `killed`: Process was terminated via kill_process API

**Notes:**
- Each process stores up to 10,000 log lines; older logs are discarded
- Process information is stored in memory only and lost on server restart

### List Processes
```
GET /list_processes
Authorization: Bearer <SANDBOX_SECRET>
```
Returns all processes (running, completed, failed, or killed).

**Response:**
```json
{
  "processes": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440000",
      "pid": 12345,
      "status": "running"
    },
    {
      "id": "660e8400-e29b-41d4-a716-446655440001",
      "pid": 12346,
      "status": "completed"
    }
  ]
}
```

### Kill Process
```
POST /kill_process
Authorization: Bearer <SANDBOX_SECRET>
Content-Type: application/json

{
  "id": "550e8400-e29b-41d4-a716-446655440000"
}
```
Terminates a running process by its unique identifier.

**Response:**
```json
{
  "success": true,
  "message": "Process killed successfully"
}
```

**Notes:**
- Sends SIGKILL to the process for immediate termination
- Cannot kill a process that has already completed, failed, or been killed

### Stream Process Logs
```
GET /process_logs_streaming
Authorization: Bearer <SANDBOX_SECRET>
```
Streams the logs (stdout and stderr) of a process in real-time using Server-Sent Events (SSE). Pass the process ID as a query parameter: `?id=<process-id>`. Historical logs are sent first, followed by new logs as they arrive.

**Response Stream:**
```
event: log
data: {"timestamp":"2025-11-04T12:34:56Z","stream":"stdout","data":"Starting application..."}

event: log
data: {"timestamp":"2025-11-04T12:34:57Z","stream":"stderr","data":"Warning: debug mode"}

event: complete
data: {"message":"stream ended"}
```

**Notes:**
- First sends all historical logs (up to 10,000 most recent lines), then streams new logs in real-time
- Both stdout and stderr are included in the stream
- The stream automatically closes when the process completes
- Multiple clients can stream logs from the same process simultaneously

### Example Workflow

```bash
# Start a background process
PROCESS_ID=$(curl -X POST http://localhost:8080/start_process \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "python -u server.py"}' \
  | jq -r '.id')

# Stream the logs in real-time
curl -X GET "http://localhost:8080/process_logs_streaming?id=$PROCESS_ID" \
  -H "Authorization: Bearer your-secret" \
  -N

# Check process status (in another terminal)
curl -X GET http://localhost:8080/list_processes \
  -H "Authorization: Bearer your-secret" \
  | jq ".processes[] | select(.id == \"$PROCESS_ID\")"

# Kill the process when done
curl -X POST http://localhost:8080/kill_process \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d "{\"id\": \"$PROCESS_ID\"}"
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
