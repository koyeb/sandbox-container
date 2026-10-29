# Sandbox Executor API Documentation

## Overview

The Sandbox Executor is an HTTP server that provides a secure API for executing commands, managing background processes, and performing file operations in a sandboxed environment. It's designed to allow controlled access to system resources through a REST API with authentication.

### Key Features

- **Command Execution:** Run one-off commands synchronously or with streaming output
- **Background Process Management:** Start, monitor, and control long-running background processes
- **File Operations:** Create, read, update, and delete files and directories; stream raw bytes of any
  size; move whole directory trees as a gzip'd tar
- **Port Binding:** Expose internal services via TCP proxy
- **Real-time Logging:** Stream process output in real-time using Server-Sent Events

## Table of Contents

### Command Execution
- [Health Check](#health-check)
- [Run Command](#run-command)
- [Run Command (Streaming)](#run-command-streaming)

### File Operations
- [Files (raw bytes)](#files-raw-bytes)
  - [Download File](#download-file)
  - [Upload File](#upload-file)
  - [Stat File](#stat-file)
  - [List Files](#list-files)
  - [Archive Directory](#archive-directory)
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

The executor supports two authentication modes:

- `SANDBOX_AUTH_MODE=static` (default): `SANDBOX_SECRET` must be set at startup
- `SANDBOX_AUTH_MODE=pool`: the server first tries to load the secret from `SANDBOX_SECRET_PATH` (default: `/var/lib/sandbox-container/sandbox-secret`); if the file does not exist yet, the first authenticated request to any protected endpoint claims that bearer token and persists it for reuse after restart

Notes:
- `/health` remains unauthenticated and never bootstraps the pool secret
- `SANDBOX_SECRET` must not be set when `SANDBOX_AUTH_MODE=pool`
- The persisted secret file is written with `0600` permissions

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
- A line ends at `\n`, `\r\n`, or `\r`: progress output redrawn in place with `\r` (e.g. `git clone --progress`) streams one `output` event per update. The line ending itself is not included in `data`
- The connection remains open until the command completes and all of its output has been sent
- Background processes started by the command (`server &`) must redirect their output (`server >/tmp/server.log 2>&1 &`), or be started with `/start_process`. A background process still holding stdout or stderr 5 seconds after the command exits is killed with SIGKILL, and the stream then completes with the command's exit code
- If the client disconnects, the command and every process it started (its whole process group, including redirected background processes) are killed
- For simple commands where buffered output is acceptable, use `/run` instead

---

### Files (raw bytes)

The `/files` routes move a file's raw bytes in the HTTP body, streamed to and from disk: they are binary-safe and
the executor's memory does not grow with the file size. They are the routes to use for new clients; the JSON routes
(`/write_file`, `/read_file`, ...) are kept unchanged for older ones.

- The path is the `path` query parameter (URL-encoded). A relative path is relative to the executor's working
  directory.
- Errors use HTTP status codes, with a JSON body `{"error": "..."}`:

| Status | When |
|---|---|
| `400 Bad Request` | missing `path`, invalid parameter or header, a body that ended early, a checksum mismatch, a directory where a file is expected (or the reverse) |
| `403 Forbidden` | permission denied |
| `404 Not Found` | the path, or a parent directory, does not exist |
| `409 Conflict` | something is in the way: a `PUT` onto a directory, a parent that is a file, or a symlink chain too long to resolve |
| `500 Internal Server Error` | any other filesystem error (e.g. disk full) |

---

### Download File

**Endpoint:** `GET /files?path=<path>` (and `HEAD`)

**Description:** Streams the file's bytes, with `Content-Type: application/octet-stream`. `HEAD` returns the same
headers without the body.

**Headers returned:** `Content-Length`, `ETag` (from size and modification time), `Last-Modified`,
`Accept-Ranges: bytes`.

**Request headers supported:**
- `Range: bytes=<start>-[<end>]`: returns `206 Partial Content` with that range (`416` if it starts past the end)
- `If-Range: <etag>`: with `Range`, returns the range only if the file still has that ETag, else the whole file
  with `200`. This is how an interrupted download resumes safely
- `If-None-Match`, `If-Modified-Since`: `304 Not Modified` when the client's copy is current

**Notes:**
- Symlinks are followed
- `400` for a directory or anything that is not a regular file (device, FIFO, socket)
- A file that reports a size of 0 (an empty file, or a pseudo-file under `/proc` or `/sys`) is streamed as it
  reads, without `Content-Length`, `ETag` or Range support: use [Stat File](#stat-file) for a size

**Example:**
```bash
# Download
curl -H "Authorization: Bearer your-secret" -o data.bin \
  "http://localhost:8080/files?path=/tmp/data.bin"

# Resume: ask for the rest of the file if it hasn't changed
curl -H "Authorization: Bearer your-secret" \
  -H "Range: bytes=1048576-" -H 'If-Range: "100000-1869a2b9c1e2f000"' \
  "http://localhost:8080/files?path=/tmp/data.bin"
```

---

### Upload File

**Endpoint:** `PUT /files?path=<path>[&mode=<octal>][&parents=1]`

**Description:** Writes the request body to the file, creating or replacing it atomically: the body goes to a
temp file next to the destination (`.<name>.koyeb-tmp-<random>`), which is synced to disk and then renamed over
the destination. A request that fails or ends early leaves the destination as it was and removes the temp file.
A crash may still orphan the temp file; the executor sweeps `.<name>.koyeb-tmp-*` files under its working
directory at startup.

**Parameters:**
- `path` (required): the file to write. If it is a symlink, its target is written (and created if it doesn't
  exist yet), like `/write_file`. A replaced file gets the executor's owner: it is a new file renamed into place
- `mode` (optional): octal permission bits, `0000` to `0777`, e.g. `0755`. Default: the existing file's mode when
  replacing one, else `0644`
- `parents=1` (optional): create missing parent directories (`0755`), like `mkdir -p`. Without it, a missing
  parent is a `404`. Only `1` is accepted; any other value is a `400`

**Request headers:**
- `X-Content-SHA256` (optional): the hex sha256 of the body. It is computed while the body streams and checked
  before the rename: a mismatch is a `400` and leaves the destination as it was

**Response:**
```json
{
  "size": 2147483648,
  "sha256": "45cd16e148159c8ca859ffb81cbfa8983cdce59fbd210f6f21b68f0c8e0d0d39"
}
```

**Example:**
```bash
curl -T data.bin -H "Authorization: Bearer your-secret" \
  -H "X-Content-SHA256: $(sha256sum data.bin | cut -d' ' -f1)" \
  "http://localhost:8080/files?path=/tmp/out/data.bin&parents=1"
```

---

### Stat File

**Endpoint:** `GET /files/stat?path=<path>[&hash=sha256]`

**Description:** Returns a file's metadata. Symlinks are followed, like a download. With `hash=sha256`, the file
is read once to compute its sha256 (regular files only).

**Response:**
```json
{
  "type": "file",
  "size": 5,
  "mode": "0644",
  "mtime": "2026-10-08T15:04:05.123456789Z",
  "sha256": "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
}
```

- `type`: `file`, `dir`, `symlink` or `other` — `symlink` only comes from `/files/list`, which does
  not follow links; stat follows them
- `mode`: the permission bits, in octal
- `mtime`: the modification time, RFC 3339 in UTC with nanoseconds

**Example:**
```bash
curl -H "Authorization: Bearer your-secret" \
  "http://localhost:8080/files/stat?path=/tmp/data.bin&hash=sha256"
```

---

### List Files

**Endpoint:** `GET /files/list?path=<dir>[&recursive=1]`

**Description:** Lists a directory as NDJSON (`Content-Type: application/x-ndjson`): one JSON object per line,
written as the directory is read, so a large tree is never built in memory. Without `recursive=1`, only the
directory's direct entries are listed. Only `1` is accepted; any other value is a `400`.

**Response:** one line per entry, in lexical order, the directory itself excluded:
```
{"path":"src","type":"dir","size":4096,"mode":"0755","mtime":"2026-10-08T15:04:05.123456789Z"}
{"path":"src/main.go","type":"file","size":1234,"mode":"0644","mtime":"2026-10-08T15:04:05.123456789Z"}
{"path":"latest","type":"symlink","size":3,"mode":"0777","mtime":"2026-10-08T15:04:05.123456789Z"}
```

- `path` is relative to the listed directory, with `/` separators
- Fields are the same as [Stat File](#stat-file), without `sha256`
- Symlinks inside the directory are listed, never followed; `path` itself may be a symlink to a directory
- `400` if `path` is not a directory. Once the listing has started, an entry that can't be read (e.g. a
  subdirectory without read permission) is listed as `{"path": "...", "size": 0, "error": "..."}` and the listing goes on

**Example:**
```bash
curl -H "Authorization: Bearer your-secret" \
  "http://localhost:8080/files/list?path=/workspace&recursive=1"
```

---

### Archive Directory

**Endpoint:** `GET /files/archive?path=<dir>[&exclude=<glob>...]` and `PUT /files/archive?path=<dir>`

**Description:** Moves a whole directory tree as one gzip'd tar, streamed in both directions with a bounded
memory footprint: a large tree is never held in memory, on either side. The directory itself may be a
symlink to one: both directions follow it, as `/files/list` does.

`GET` responds with `Content-Type: application/gzip` and a tar of the directory:

- Entry names are the paths relative to the directory, `/`-separated; the directory itself is not an entry
- `exclude` patterns (repeat the parameter for several) match any suffix of a relative path at a `/`
  boundary, as `tar --exclude` does: `node_modules` drops `sub/node_modules` too, `*.log` drops `.log`
  files at any depth. A matching directory is skipped whole; a malformed pattern is a `400`
- Symlinks are archived as links, not followed; special files (FIFOs, sockets, devices) are skipped.
  Hardlinked paths are archived as independent copies: the extractor's hardlink support covers foreign
  archives
- The walk is streamed. A failure before the first entry is answered with the error status and JSON
  body; past it, the response is a `200` that ends short, without a gzip footer or tar trailer, so the
  client's decompressor reports the truncation — a partial archive never looks complete
- `HEAD` is answered like `GET`, without the body: the walk still runs and sets the same headers

`PUT` extracts the archive's members under `<dir>`, creating it if missing, merging if it exists, and
answers:

```json
{"files": 2, "bytes": 4096}
```

`files` counts regular files and hardlinks, `bytes` the content bytes written.

- Every member name must stay inside `<dir>` after cleaning: absolute names, `..`, absolute link
  targets and links whose target, read from the entry's directory, leaves the directory are refused
  with `400`. The check is
  on the target's own path — a pre-existing symlink inside `<dir>` that the target names is stored,
  not followed — and the refused member itself is never created (its parent directories may
  already have been, and members
  before it stay)
- Modes and mtimes are kept (directory modes and mtimes are applied after the directory's contents,
  like tar); symlinks are stored byte-identical to the archive, but a symlink's mtime cannot be
  restored — the standard library has no way to set it
- GNU-tar shapes work: `./`-prefixed entries (`tar czf x.tgz -C dir .`), PAX global headers, missing
  parent directory members, and hardlinks (the `Linkname` is the archive-root member path). FIFOs,
  devices and other unsupported member types are a `400`; a hardlink whose target is a directory or
  a link is a `409`, and one whose target is not on disk when the link is read (tar orders targets
  first) is a `400`
- Members replace what is at their own path, as tar does — a stale file or link is removed first, never
  written through, so re-extracting an archive over its own output works. A directory member merges
  instead of replacing: a non-directory at its path is a `409`. A symlink in a parent
  component's way is a `409`, whatever created it: extraction never writes through a parent link.
  Archives this route's own GET produces never hit that — their links are members, not parents
- Extraction is not atomic: a body that is not gzip at all is a `400` before anything is created; a
  body that ends mid-stream leaves the members already extracted (as tar does) and answers `400`. A
  `PUT` whose `path` exists as a file is a `409`: the archive root must be a directory

**Examples:**
```bash
curl -H "Authorization: Bearer your-secret" \
  "http://localhost:8080/files/archive?path=/workspace&exclude=node_modules&exclude=*.log" \
  -o workspace.tgz

curl -X PUT -H "Authorization: Bearer your-secret" \
  --data-binary @workspace.tgz \
  "http://localhost:8080/files/archive?path=/workspace"
```

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
- The TCP proxy listens on `SANDBOX_PROXY_PORT` (default: 3031) and forwards traffic to the specified internal port
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
- `206 Partial Content`: A range of a file (`GET /files` with `Range`)
- `400 Bad Request`: Invalid request body or parameters
- `401 Unauthorized`: Missing or invalid authentication token
- `403 Forbidden`: Permission denied on a file (`/files` routes)
- `404 Not Found`: The file or directory does not exist (`/files` routes)
- `405 Method Not Allowed`: Wrong HTTP method used
- `409 Conflict`: Resource conflict (e.g., port already bound, a directory in the way of a file)
- `416 Range Not Satisfiable`: A `Range` that starts past the end of the file
- `500 Internal Server Error`: Server-side error during operation

Error responses include descriptive error messages in the response body. The `/files` routes always return them
as JSON, `{"error": "..."}`; the JSON file routes (`/write_file`, `/read_file`, ...) answer `200` with an `error`
field instead.

## Security Considerations

- All file and directory operations are performed with the permissions of the user running the server
- The file routes (`/files`, `/files/stat`, `/files/list`, `/files/archive` and the legacy JSON file routes) read and write any path on the executor's filesystem, gated only by the bearer token — by design, like `/run`: treat the token as full filesystem access
- Command execution uses `sh -c`, allowing shell features but also potential security risks
- The server should be run in a properly isolated environment (container, VM, etc.)
- The sandbox secret should be kept confidential and rotated regularly
- In `pool` mode, mount persistent storage for `SANDBOX_SECRET_PATH` if the secret must survive container restarts
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
