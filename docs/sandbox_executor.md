# Sandbox Executor API Documentation

## Overview

The Sandbox Executor is an HTTP server that provides a secure API for executing commands and performing file operations in a sandboxed environment. It's designed to allow controlled access to system resources through a REST API with authentication.

## Authentication

All endpoints (except `/health`) require authentication via a bearer token passed in the `Authorization` header:

```
Authorization: Bearer <sandbox-secret>
```

## API Endpoints

### Health Check

**Endpoint:** `GET /health`

**Description:** Returns the health status of the server.

**Authentication:** Not required

**Response:**
```json
{
  "status": "ok"
}
```

---

### Run Command

**Endpoint:** `POST /run`

**Description:** Executes a shell command in the sandbox environment.

**Request Body:**
```json
{
  "cmd": "echo 'Hello World'",
  "cwd": "/path/to/working/directory",
  "env": {
    "VAR_NAME": "value",
    "ANOTHER_VAR": "another_value"
  }
}
```

**Parameters:**
- `cmd` (string, required): The shell command to execute
- `cwd` (string, optional): Working directory for the command execution
- `env` (object, optional): Environment variables to set/override for the command

**Response:**
```json
{
  "stdout": "command output",
  "stderr": "error output",
  "error": "Non-zero exit code",
  "code": 0
}
```

**Response Fields:**
- `stdout` (string): Standard output from the command
- `stderr` (string): Standard error output from the command
- `error` (string): Error message if command failed (only present on failure)
- `code` (int): Exit code of the command

**Example:**
```bash
curl -X POST http://localhost:8080/run \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "cmd": "ls -la",
    "cwd": "/tmp",
    "env": {"DEBUG": "true"}
  }'
```

---

### Run Command (Streaming)

**Endpoint:** `POST /run_streaming`

**Description:** Executes a shell command in the sandbox environment and streams the output in real-time using Server-Sent Events (SSE).

**Request Body:**
```json
{
  "cmd": "echo 'Hello World'",
  "cwd": "/path/to/working/directory",
  "env": {
    "VAR_NAME": "value",
    "ANOTHER_VAR": "another_value"
  }
}
```

**Parameters:**
- `cmd` (string, required): The shell command to execute
- `cwd` (string, optional): Working directory for the command execution
- `env` (object, optional): Environment variables to set/override for the command

**Response:** Server-Sent Events stream with the following event types:

1. **output** events (sent as command produces output):
```json
{
  "stream": "stdout",
  "data": "line of output"
}
```
or
```json
{
  "stream": "stderr",
  "data": "line of error output"
}
```

2. **complete** event (sent when command finishes):
```json
{
  "code": 0,
  "error": false
}
```

3. **error** event (sent if command fails to start):
```json
{
  "error": "error message"
}
```

**Response Format:**
- Uses Server-Sent Events (SSE) protocol
- Content-Type: `text/event-stream`
- Each event follows SSE format: `event: <type>\ndata: <json>\n\n`
- Connection stays open until command completes

**Example:**
```bash
curl -X POST http://localhost:8080/run_streaming \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -N \
  -d '{
    "cmd": "for i in 1 2 3; do echo $i; sleep 1; done",
    "cwd": "/tmp"
  }'
```

**Example Response Stream:**
```
event: output
data: {"stream":"stdout","data":"1"}

event: output
data: {"stream":"stdout","data":"2"}

event: output
data: {"stream":"stdout","data":"3"}

event: complete
data: {"code":0,"error":false}

```

**Notes:**
- Use this endpoint for long-running commands where you want real-time output
- stdout and stderr are streamed line-by-line as they are produced
- Both streams are processed concurrently
- The connection remains open until the command completes
- For simple commands where buffered output is acceptable, use `/run` instead

---

### Write File

**Endpoint:** `POST /write_file`

**Description:** Creates or overwrites a file with the specified content.

**Request Body:**
```json
{
  "path": "/path/to/file.txt",
  "content": "file content here"
}
```

**Parameters:**
- `path` (string, required): The file path to write to
- `content` (string, required): The content to write to the file

**Response:**
```json
{
  "success": true,
  "error": "error message if failed"
}
```

**Example:**
```bash
curl -X POST http://localhost:8080/write_file \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "path": "/tmp/test.txt",
    "content": "Hello, World!"
  }'
```

---

### Read File

**Endpoint:** `POST /read_file`

**Description:** Reads the content of a file.

**Request Body:**
```json
{
  "path": "/path/to/file.txt"
}
```

**Parameters:**
- `path` (string, required): The file path to read from

**Response:**
```json
{
  "content": "file content",
  "error": "error message if failed"
}
```

**Example:**
```bash
curl -X POST http://localhost:8080/read_file \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "path": "/tmp/test.txt"
  }'
```

---

### Delete File

**Endpoint:** `POST /delete_file`

**Description:** Deletes a single file.

**Request Body:**
```json
{
  "path": "/path/to/file.txt"
}
```

**Parameters:**
- `path` (string, required): The file path to delete

**Response:**
```json
{
  "success": true,
  "error": "error message if failed"
}
```

**Example:**
```bash
curl -X POST http://localhost:8080/delete_file \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "path": "/tmp/test.txt"
  }'
```

---

### Make Directory

**Endpoint:** `POST /make_dir`

**Description:** Creates a directory, including any necessary parent directories.

**Request Body:**
```json
{
  "path": "/path/to/directory"
}
```

**Parameters:**
- `path` (string, required): The directory path to create

**Response:**
```json
{
  "success": true,
  "error": "error message if failed"
}
```

**Notes:**
- Creates parent directories if they don't exist (equivalent to `mkdir -p`)
- Directory permissions are set to 0755

**Example:**
```bash
curl -X POST http://localhost:8080/make_dir \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "path": "/tmp/my/nested/directory"
  }'
```

---

### Delete Directory

**Endpoint:** `POST /delete_dir`

**Description:** Recursively deletes a directory and all its contents.

**Request Body:**
```json
{
  "path": "/path/to/directory"
}
```

**Parameters:**
- `path` (string, required): The directory path to delete

**Response:**
```json
{
  "success": true,
  "error": "error message if failed"
}
```

**Notes:**
- Recursively removes all files and subdirectories (equivalent to `rm -rf`)
- Use with caution as this operation cannot be undone

**Example:**
```bash
curl -X POST http://localhost:8080/delete_dir \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "path": "/tmp/my/directory"
  }'
```

---

### List Directory

**Endpoint:** `POST /list_dir`

**Description:** Lists the contents of a directory.

**Request Body:**
```json
{
  "path": "/path/to/directory"
}
```

**Parameters:**
- `path` (string, required): The directory path to list

**Response:**
```json
{
  "entries": ["file1.txt", "file2.txt", "subdir"],
  "error": "error message if failed"
}
```

**Response Fields:**
- `entries` (array of strings): List of file and directory names in the specified directory
- `error` (string): Error message if the operation failed

**Notes:**
- Returns only the names of entries, not full paths
- Does not distinguish between files and directories in the response
- Does not recursively list subdirectories

**Example:**
```bash
curl -X POST http://localhost:8080/list_dir \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "path": "/tmp"
  }'
```

---

### Bind Port

**Endpoint:** `POST /bind_port`

**Description:** Configures the TCP proxy to forward traffic to a specified port inside the sandbox. This allows you to expose services running inside the sandbox to external connections.

**Request Body:**
```json
{
  "port": "8080"
}
```

**Parameters:**
- `port` (string, required): The port number to bind to (as a string)

**Response:**
```json
{
  "success": true,
  "message": "Port binding configured",
  "port": "8080"
}
```

**Response Fields:**
- `success` (boolean): Whether the operation succeeded
- `message` (string): Confirmation message
- `port` (string): The port that was bound

**Error Response (Port Already Bound):**
```json
{
  "success": false,
  "error": "Port already bound",
  "current_port": "8080"
}
```
Returns HTTP 409 Conflict status code.

**Notes:**
- The TCP proxy listens on `PROXY_PORT` (default: 3031) and forwards traffic to the specified internal port
- Only one port binding can be active at a time; attempting to bind when a port is already bound will return an error
- You must unbind the current port before binding a new one
- The port must be available and accessible within the sandbox environment

**Example:**
```bash
curl -X POST http://localhost:8080/bind_port \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "port": "8080"
  }'
```

---

### Unbind Port

**Endpoint:** `POST /unbind_port`

**Description:** Removes the TCP proxy port binding, stopping traffic forwarding to the previously bound port.

**Request Body:** None required

**Response:**
```json
{
  "success": true,
  "message": "Port binding removed"
}
```

**Response Fields:**
- `success` (boolean): Whether the operation succeeded
- `message` (string): Confirmation message

**Notes:**
- This endpoint unbinds any currently bound port
- No parameters are required
- After unbinding, the TCP proxy will no longer forward traffic

**Example:**
```bash
curl -X POST http://localhost:8080/unbind_port \
  -H "Authorization: Bearer your-secret"
```

---

## Error Handling

All endpoints return appropriate HTTP status codes:

- `200 OK`: Request succeeded
- `400 Bad Request`: Invalid request body or parameters
- `401 Unauthorized`: Missing or invalid authentication token
- `500 Internal Server Error`: Server-side error during operation

Error responses include descriptive error messages in the response body.

## Security Considerations

- All file and directory operations are performed with the permissions of the user running the server
- Command execution uses `sh -c`, allowing shell features but also potential security risks
- The server should be run in a properly isolated environment (container, VM, etc.)
- The sandbox secret should be kept confidential and rotated regularly
- Consider implementing additional path restrictions to prevent access to sensitive directories
