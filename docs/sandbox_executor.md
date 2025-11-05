# Sandbox Executor API Documentation

## Overview

The Sandbox Executor is an HTTP server that provides a secure API for executing commands, managing background processes, and performing file operations in a sandboxed environment. It's designed to allow controlled access to system resources through a REST API with authentication.

### Key Features

- **Command Execution:** Run one-off commands synchronously or with streaming output
- **Background Process Management:** Start, monitor, and control long-running background processes
- **File Operations:** Create, read, update, and delete files and directories
- **Port Binding:** Expose internal services via TCP proxy
- **Real-time Logging:** Stream process output in real-time using Server-Sent Events

## Table of Contents

### Command Execution
- [Health Check](#health-check)
- [Run Command](#run-command)
- [Run Command (Streaming)](#run-command-streaming)

### File Operations
- [Write File](#write-file)
- [Read File](#read-file)
- [Delete File](#delete-file)
- [Make Directory](#make-directory)
- [Delete Directory](#delete-directory)
- [List Directory](#list-directory)

### Port Management
- [Bind Port](#bind-port)
- [Unbind Port](#unbind-port)

### Background Process Management
- [Start Process](#start-process)
- [List Processes](#list-processes)
- [Kill Process](#kill-process)
- [Stream Process Logs](#stream-process-logs)
- [Process Management Workflow](#background-process-management-workflow)

### Reference
- [Error Handling](#error-handling)
- [Security Considerations](#security-considerations)

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

### Start Process

**Endpoint:** `POST /start_process`

**Description:** Starts a new background process in the sandbox environment. The process runs asynchronously and can be monitored and controlled via other process management endpoints.

**Request Body:**
```json
{
  "cmd": "python -u app.py",
  "cwd": "/path/to/working/directory",
  "env": {
    "PORT": "8080",
    "DEBUG": "true"
  }
}
```

**Parameters:**
- `cmd` (string, required): The shell command to execute in the background
- `cwd` (string, optional): Working directory for the command execution
- `env` (object, optional): Environment variables to set/override for the command

**Response (201 Created):**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "pid": 12345,
  "status": "running"
}
```

**Response Fields:**
- `id` (string): Unique UUID identifier for the process
- `pid` (integer): Operating system process ID
- `status` (string): Current process status (always "running" on successful start)

**Error Response (500 Internal Server Error):**
```json
{
  "error": "Failed to start process: <error details>"
}
```

**Process Status Values:**
- `running`: Process is currently executing
- `completed`: Process exited successfully (exit code 0)
- `failed`: Process exited with non-zero exit code
- `killed`: Process was terminated via kill_process API

**Notes:**
- The process runs in the background and does not block the API response
- Process output (stdout/stderr) is captured and can be accessed via `/process_logs_streaming`
- Each process stores up to 10,000 log lines; older logs are discarded
- Environment variables are added to the existing environment inherited from the server
- Use unique process IDs to manage and monitor processes

**Example:**
```bash
curl -X POST http://localhost:8080/start_process \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "cmd": "python -u app.py",
    "cwd": "/home/user/project",
    "env": {
      "PORT": "8080",
      "DEBUG": "true"
    }
  }'
```

---

### List Processes

**Endpoint:** `GET /list_processes`

**Description:** Returns a list of all processes (running, completed, failed, or killed) managed by the sandbox executor.

**Request Body:** None

**Response (200 OK):**
```json
{
  "processes": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440000",
      "pid": 12345,
      "status": "running",
      "command": "python train.py"
    },
    {
      "id": "660e8400-e29b-41d4-a716-446655440001",
      "pid": 12346,
      "status": "completed",
      "command": "npm install"
    },
    {
      "id": "770e8400-e29b-41d4-a716-446655440002",
      "pid": 12347,
      "status": "killed",
      "command": "sleep 1000"
    }
  ]
}
```

**Response Fields:**
  - `id` (string): Unique UUID identifier for the process
  - `pid` (integer): Operating system process ID
  - `status` (string): Current process status
  - `command` (string): The command that was executed

**Notes:**
- Returns all processes regardless of status
- Completed processes remain in the list until the server restarts
- No pagination is implemented; all processes are returned
- Processes are stored in memory only and lost on server restart

**Example:**
```bash
curl -X GET http://localhost:8080/list_processes \
  -H "Authorization: Bearer your-secret"
```

---

### Kill Process

**Endpoint:** `POST /kill_process`

**Description:** Terminates a running process by its unique identifier.

**Request Body:**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000"
}
```

**Parameters:**
- `id` (string, required): The unique process ID returned by `/start_process`

**Response (200 OK):**
```json
{
  "success": true,
  "message": "Process killed successfully"
}
```

**Response Fields:**
- `success` (boolean): Whether the operation succeeded
- `message` (string): Confirmation message

**Error Response (400 Bad Request):**
```json
{
  "success": false,
  "error": "process not found: <process-id>"
}
```
or
```json
{
  "success": false,
  "error": "process is not running (status: completed)"
}
```

**Notes:**
- Sends SIGKILL to the process for immediate termination
- Cannot kill a process that has already completed, failed, or been killed
- The process status will be updated to "killed" after successful termination
- Child processes may become orphaned if not properly managed by the parent

**Example:**
```bash
curl -X POST http://localhost:8080/kill_process \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "550e8400-e29b-41d4-a716-446655440000"
  }'
```

---

### Stream Process Logs

**Endpoint:** `GET /process_logs_streaming`

**Description:** Streams the logs (stdout and stderr) of a process in real-time using Server-Sent Events (SSE). The process ID is passed as a query parameter. Historical logs are sent first, followed by new logs as they arrive.

**Query Parameters:**
- `id` (string, required): The unique process ID returned by `/start_process`

**Example URL:**
```
GET /process_logs_streaming?id=550e8400-e29b-41d4-a716-446655440000
```

**Response (200 OK):** Server-Sent Events stream with log entries

**Event Format:**

1. **log** events (sent for each log line):
```json
{
  "timestamp": "2025-11-04T12:34:56Z",
  "stream": "stdout",
  "data": "Application started on port 8080"
}
```
or
```json
{
  "timestamp": "2025-11-04T12:34:57Z",
  "stream": "stderr",
  "data": "Warning: debug mode enabled"
}
```

2. **complete** event (sent when stream ends):
```json
{
  "message": "stream ended"
}
```

3. **error** event (sent if process not found):
```json
{
  "error": "process not found: <process-id>"
}
```

**Log Entry Fields:**
- `timestamp` (string): ISO 8601 timestamp when the log was captured
- `stream` (string): Either "stdout" or "stderr"
- `data` (string): The log line content

**Response Format:**
- Uses Server-Sent Events (SSE) protocol
- Content-Type: `text/event-stream`
- Each event follows SSE format: `event: <type>\ndata: <json>\n\n`
- Connection stays open until the process completes or client disconnects

**Example:**
```bash
curl -X GET "http://localhost:8080/process_logs_streaming?id=550e8400-e29b-41d4-a716-446655440000" \
  -H "Authorization: Bearer your-secret" \
  -N
```

**Example Response Stream:**
```
event: log
data: {"timestamp":"2025-11-04T12:34:56Z","stream":"stdout","data":"Starting application..."}

event: log
data: {"timestamp":"2025-11-04T12:34:57Z","stream":"stdout","data":"Server listening on port 8080"}

event: log
data: {"timestamp":"2025-11-04T12:34:58Z","stream":"stderr","data":"Warning: debug mode"}

event: complete
data: {"message":"stream ended"}

```

**Notes:**
- First sends all historical logs (up to 10,000 most recent lines), then streams new logs in real-time
- Both stdout and stderr are included in the stream
- Logs are timestamped at capture time, not when streamed
- The stream automatically closes when the process completes
- Multiple clients can stream logs from the same process simultaneously
- If the process has already completed, you'll receive all captured logs followed by the complete event

**JavaScript Example:**
```javascript
const processId = '550e8400-e29b-41d4-a716-446655440000';
const response = await fetch(`http://localhost:8080/process_logs_streaming?id=${processId}`, {
  method: 'GET',
  headers: {
    'Authorization': 'Bearer your-secret',
  }
});

const reader = response.body.getReader();
const decoder = new TextDecoder();

while (true) {
  const { done, value } = await reader.read();
  if (done) break;
  
  const text = decoder.decode(value);
  const lines = text.split('\n');
  
  for (const line of lines) {
    if (line.startsWith('data: ')) {
      const data = JSON.parse(line.substring(6));
      if (data.stream) {
        console.log(`[${data.stream}] ${data.data}`);
      }
    }
  }
}
```

**Python Example:**
```python
import requests
import json

process_id = '550e8400-e29b-41d4-a716-446655440000'

response = requests.get(
    f'http://localhost:8080/process_logs_streaming?id={process_id}',
    headers={
        'Authorization': 'Bearer your-secret'
    },
    stream=True
)

for line in response.iter_lines():
    if line:
        line = line.decode('utf-8')
        if line.startswith('data: '):
            data = json.loads(line[6:])
            if 'stream' in data:
                print(f"[{data['stream']}] {data['data']}")
```

---

## Background Process Management Workflow

### Starting and Monitoring a Long-Running Process

1. **Start the process:**
```bash
PROCESS_ID=$(curl -X POST http://localhost:8080/start_process \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "python -u server.py"}' \
  | jq -r '.id')

echo "Started process: $PROCESS_ID"
```

2. **Stream the logs in real-time:**
```bash
curl -X POST http://localhost:8080/process_logs_streaming \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -N \
  -d "{\"id\": \"$PROCESS_ID\"}"
```

3. **Check process status (in another terminal):**
```bash
curl -X GET http://localhost:8080/list_processes \
  -H "Authorization: Bearer your-secret" \
  | jq ".processes[] | select(.id == \"$PROCESS_ID\")"
```

4. **Kill the process when done:**
```bash
curl -X POST http://localhost:8080/kill_process \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d "{\"id\": \"$PROCESS_ID\"}"
```

### Running Multiple Concurrent Processes

```bash
# Start multiple background processes
WORKER_1=$(curl -s -X POST http://localhost:8080/start_process \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "python worker.py --task=1"}' | jq -r '.id')

WORKER_2=$(curl -s -X POST http://localhost:8080/start_process \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{"cmd": "python worker.py --task=2"}' | jq -r '.id')

# Monitor all processes
curl -X GET http://localhost:8080/list_processes \
  -H "Authorization: Bearer your-secret" | jq

# Stream logs from specific worker
curl -X GET "http://localhost:8080/process_logs_streaming?id=$WORKER_1" \
  -H "Authorization: Bearer your-secret" \
  -N
```

---

## Error Handling

All endpoints return appropriate HTTP status codes:

- `200 OK`: Request succeeded
- `201 Created`: Resource created successfully (e.g., process started)
- `400 Bad Request`: Invalid request body or parameters
- `401 Unauthorized`: Missing or invalid authentication token
- `405 Method Not Allowed`: Wrong HTTP method used
- `409 Conflict`: Resource conflict (e.g., port already bound)
- `500 Internal Server Error`: Server-side error during operation

Error responses include descriptive error messages in the response body.

## Security Considerations

- All file and directory operations are performed with the permissions of the user running the server
- Command execution uses `sh -c`, allowing shell features but also potential security risks
- The server should be run in a properly isolated environment (container, VM, etc.)
- The sandbox secret should be kept confidential and rotated regularly
- Consider implementing additional path restrictions to prevent access to sensitive directories

### Background Process Security

- **Process Isolation:** Background processes run with the same permissions as the sandbox executor
- **Resource Limits:** No automatic resource limits (CPU, memory) are enforced on background processes
- **Process Cleanup:** Completed processes remain in memory until server restart; consider monitoring memory usage
- **Log Storage:** Each process stores up to 10,000 log lines in memory; very verbose processes may lose older logs
- **Process Persistence:** All process information is stored in memory only and lost on server restart
- **Orphaned Processes:** If the sandbox executor crashes, background processes may continue running as orphans
- **Concurrent Access:** The process management system is thread-safe and supports concurrent API calls

### Best Practices for Process Management

1. **Monitor Running Processes:** Regularly check `/list_processes` to avoid accumulating too many processes
2. **Clean Up:** Kill processes that are no longer needed to free system resources
3. **Log Management:** For very verbose processes, be aware of the 10,000 line log buffer limit
4. **Error Handling:** Always check process status after starting to ensure successful launch
5. **Timeouts:** Implement client-side timeouts when streaming logs to prevent hung connections
6. **Resource Monitoring:** Monitor system resources (CPU, memory) as no automatic limits are enforced
