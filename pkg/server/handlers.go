package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/koyeb/sandbox-container/pkg/logger"
)

// sseWriter provides thread-safe writing for Server-Sent Events
type sseWriter struct {
	w       http.ResponseWriter
	mu      sync.Mutex
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming unsupported")
	}
	return &sseWriter{
		w:       w,
		flusher: flusher,
	}, nil
}

func (s *sseWriter) writeEvent(event, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	s.flusher.Flush()
}

func (s *sseWriter) writeEventf(event, format string, args ...interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "event: %s\ndata: ", event)
	fmt.Fprintf(s.w, format, args...)
	fmt.Fprintf(s.w, "\n\n")
	s.flusher.Flush()
}

type RunRequest struct {
	Cmd string            `json:"cmd"`
	Cwd string            `json:"cwd,omitempty"`
	Env map[string]string `json:"env,omitempty"`
}

type RunResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error,omitempty"`
	Code   int    `json:"code"`
}

type WriteFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type ReadFileRequest struct {
	Path string `json:"path"`
}

type ReadFileResponse struct {
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

type DeleteFileRequest struct {
	Path string `json:"path"`
}

type DeleteDirRequest struct {
	Path string `json:"path"`
}

type MakeDirRequest struct {
	Path string `json:"path"`
}

type ListDirRequest struct {
	Path string `json:"path"`
}

type ListDirResponse struct {
	Entries []string `json:"entries,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	logger.Trace("Health check request", "method", r.Method, "remote_addr", r.RemoteAddr)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) runHandler(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	slog.Debug("Executing command", "cmd", req.Cmd, "cwd", req.Cwd, "env", req.Env)

	cmd := exec.Command("sh", "-c", req.Cmd)

	// Set working directory if provided
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}

	// Set environment variables if provided
	if len(req.Env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range req.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Debug("Failed to get stdout pipe", "error", err)
		http.Error(w, "Failed to get stdout", http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		slog.Debug("Failed to get stderr pipe", "error", err)
		http.Error(w, "Failed to get stderr", http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		slog.Debug("Failed to start command", "cmd", req.Cmd, "error", err)
		http.Error(w, "Failed to start command", http.StatusInternalServerError)
		return
	}
	outBytes, _ := io.ReadAll(stdout)
	errBytes, _ := io.ReadAll(stderr)
	cmd.Wait()

	exitCode := cmd.ProcessState.ExitCode()
	slog.Debug("Command completed",
		"cmd", req.Cmd,
		"exit_code", exitCode,
		"stdout", string(outBytes),
		"stderr", string(errBytes))

	resp := RunResponse{
		Stdout: string(outBytes),
		Stderr: string(errBytes),
		Code:   exitCode,
	}
	if exitCode != 0 {
		resp.Error = "Non-zero exit code"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Process management handlers

type StartProcessRequest struct {
	Cmd string            `json:"cmd"`
	Cwd string            `json:"cwd,omitempty"`
	Env map[string]string `json:"env,omitempty"`
}

type StartProcessResponse struct {
	ID     string `json:"id"`
	PID    int    `json:"pid"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) startProcessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req StartProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if req.Cmd == "" {
		http.Error(w, "Command is required", http.StatusBadRequest)
		return
	}

	slog.Debug("Start process request", "cmd", req.Cmd, "cwd", req.Cwd, "env", req.Env)

	process, err := s.processManager.StartProcess(req.Cmd, req.Cwd, req.Env)
	if err != nil {
		slog.Debug("Failed to start process", "cmd", req.Cmd, "error", err)
		resp := StartProcessResponse{
			Error: err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(resp)
		return
	}

	slog.Debug("Process started via API", "id", process.ID, "pid", process.PID, "cmd", req.Cmd)

	resp := StartProcessResponse{
		ID:     process.ID,
		PID:    process.PID,
		Status: string(process.Status),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

type ListProcessesResponse struct {
	Processes []map[string]interface{} `json:"processes"`
}

func (s *Server) listProcessesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	slog.Debug("Listing processes")

	processes := s.processManager.ListProcesses()

	processesData := make([]map[string]interface{}, len(processes))
	for i, p := range processes {
		processesData[i] = p.ToSummaryJSON()
	}

	slog.Debug("Processes listed", "count", len(processes))

	resp := ListProcessesResponse{
		Processes: processesData,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type KillProcessRequest struct {
	ID string `json:"id"`
}

type KillProcessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (s *Server) killProcessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req KillProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if req.ID == "" {
		http.Error(w, "Process ID is required", http.StatusBadRequest)
		return
	}

	slog.Debug("Kill process request", "id", req.ID)

	err := s.processManager.KillProcess(req.ID)
	if err != nil {
		slog.Debug("Failed to kill process", "id", req.ID, "error", err)
		resp := KillProcessResponse{
			Success: false,
			Error:   err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(resp)
		return
	}

	slog.Debug("Process killed successfully via API", "id", req.ID)

	resp := KillProcessResponse{
		Success: true,
		Message: "Process killed successfully",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) processLogsStreamingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get process ID from query parameter
	processID := r.URL.Query().Get("id")
	if processID == "" {
		http.Error(w, "Process ID is required", http.StatusBadRequest)
		return
	}

	slog.Debug("Streaming process logs request", "id", processID)

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	writer, err := newSSEWriter(w)
	if err != nil {
		slog.Debug("Failed to create SSE writer for process logs", "id", processID, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logChan, err := s.processManager.StreamProcessLogs(processID)
	if err != nil {
		slog.Debug("Failed to stream process logs", "id", processID, "error", err)
		writer.writeEventf("error", "{\"error\": \"%s\"}", err.Error())
		return
	}

	slog.Debug("Started streaming process logs", "id", processID)

	// Stream logs as they arrive
	logCount := 0
	for entry := range logChan {
		data, _ := json.Marshal(entry)
		writer.writeEvent("log", string(data))
		logCount++
	}

	slog.Debug("Process logs stream ended", "id", processID, "logs_sent", logCount)

	// Send completion event
	writer.writeEvent("complete", "{\"message\": \"stream ended\"}")
}

func (s *Server) runStreamingHandler(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	slog.Debug("Executing streaming command", "cmd", req.Cmd, "cwd", req.Cwd, "env", req.Env)

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	writer, err := newSSEWriter(w)
	if err != nil {
		slog.Debug("Failed to create SSE writer", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create context for goroutine lifecycle management
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", req.Cmd)

	// Set working directory if provided
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}

	// Set environment variables if provided
	if len(req.Env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range req.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Debug("Failed to get stdout pipe for streaming", "error", err)
		writer.writeEvent("error", "{\"error\": \"Failed to get stdout\"}")
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		slog.Debug("Failed to get stderr pipe for streaming", "error", err)
		writer.writeEvent("error", "{\"error\": \"Failed to get stderr\"}")
		return
	}

	if err = cmd.Start(); err != nil {
		slog.Debug("Failed to start streaming command", "cmd", req.Cmd, "error", err)
		writer.writeEvent("error", "{\"error\": \"Failed to start command\"}")
		return
	}

	// WaitGroup to track completion of both stdout and stderr goroutines
	var wg sync.WaitGroup

	streamOutput := func(r io.Reader, stream string) {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				slog.Debug("Command output", "cmd", req.Cmd, "stream", stream, "bytes", n)
				data, _ := json.Marshal(map[string]string{"stream": stream, "data": string(buf[:n])})
				writer.writeEvent("output", string(data))
			}
			if err != nil {
				if err != io.EOF {
					slog.Debug("read error", "stream", stream, "error", err)
				}
				return
			}
		}
	}

	// Stream stdout
	wg.Go(func() { streamOutput(stdout, "stdout") })

	// Stream stderr
	wg.Go(func() { streamOutput(stderr, "stderr") })

	// Wait for command to finish
	err = cmd.Wait()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	slog.Debug("Streaming command completed", "cmd", req.Cmd, "exit_code", exitCode)

	// Wait for both stdout and stderr goroutines to finish processing all output
	wg.Wait()

	// Send completion event
	completeData, _ := json.Marshal(map[string]interface{}{
		"code":  exitCode,
		"error": err != nil,
	})
	writer.writeEvent("complete", string(completeData))
}

type BindPortRequest struct {
	Port string `json:"port"`
}

func (s *Server) bindPortHandler(w http.ResponseWriter, r *http.Request) {
	var req BindPortRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if req.Port == "" {
		http.Error(w, "Port is required", http.StatusBadRequest)
		return
	}

	slog.Debug("Binding port", "port", req.Port)

	// Check if a port is already bound
	currentPort := s.tcpProxy.GetTargetPort()
	if currentPort != "" {
		slog.Debug("Port already bound", "current_port", currentPort, "requested_port", req.Port)
		resp := map[string]interface{}{
			"success":      false,
			"error":        "Port already bound",
			"current_port": currentPort,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(resp)
		return
	}

	s.tcpProxy.SetTargetPort(req.Port)
	slog.Debug("Port bound successfully", "port", req.Port)

	resp := map[string]interface{}{
		"success": true,
		"message": "Port binding configured",
		"port":    req.Port,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) unbindPortHandler(w http.ResponseWriter, r *http.Request) {
	currentPort := s.tcpProxy.GetTargetPort()
	slog.Debug("Unbinding port", "current_port", currentPort)

	s.tcpProxy.ClearTargetPort()
	slog.Debug("Port unbound successfully")

	resp := map[string]interface{}{
		"success": true,
		"message": "Port binding removed",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) deleteDirHandler(w http.ResponseWriter, r *http.Request) {
	var req DeleteDirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	slog.Debug("Deleting directory", "path", req.Path)

	err := os.RemoveAll(req.Path)
	resp := map[string]interface{}{"success": err == nil}
	if err != nil {
		slog.Debug("Failed to delete directory", "path", req.Path, "error", err)
		resp["error"] = err.Error()
	} else {
		slog.Debug("Directory deleted successfully", "path", req.Path)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) makeDirHandler(w http.ResponseWriter, r *http.Request) {
	var req MakeDirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	slog.Debug("Creating directory", "path", req.Path)

	err := os.MkdirAll(req.Path, 0o755)
	resp := map[string]interface{}{"success": err == nil}
	if err != nil {
		slog.Debug("Failed to create directory", "path", req.Path, "error", err)
		resp["error"] = err.Error()
	} else {
		slog.Debug("Directory created successfully", "path", req.Path)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) listDirHandler(w http.ResponseWriter, r *http.Request) {
	var req ListDirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	slog.Debug("Listing directory", "path", req.Path)

	entries, err := os.ReadDir(req.Path)
	resp := ListDirResponse{}
	if err != nil {
		slog.Debug("Failed to list directory", "path", req.Path, "error", err)
		resp.Error = err.Error()
	} else {
		resp.Entries = make([]string, len(entries))
		for i, entry := range entries {
			resp.Entries[i] = entry.Name()
		}
		slog.Debug("Directory listed successfully", "path", req.Path, "entries", len(entries))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) writeFileHandler(w http.ResponseWriter, r *http.Request) {
	var req WriteFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	contentLen := len(req.Content)
	slog.Debug("Writing file", "path", req.Path, "content_length", contentLen)

	err := os.WriteFile(req.Path, []byte(req.Content), 0o644)
	resp := map[string]interface{}{"success": err == nil}
	if err != nil {
		slog.Debug("Failed to write file", "path", req.Path, "error", err)
		resp["error"] = err.Error()
	} else {
		slog.Debug("File written successfully", "path", req.Path, "bytes", contentLen)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) readFileHandler(w http.ResponseWriter, r *http.Request) {
	var req ReadFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	slog.Debug("Reading file", "path", req.Path)

	content, err := os.ReadFile(req.Path)
	resp := ReadFileResponse{}
	if err != nil {
		slog.Debug("Failed to read file", "path", req.Path, "error", err)
		resp.Error = err.Error()
	} else {
		slog.Debug("File read successfully", "path", req.Path, "bytes", len(content))
		resp.Content = string(content)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) deleteFileHandler(w http.ResponseWriter, r *http.Request) {
	var req DeleteFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	slog.Debug("Deleting file", "path", req.Path)

	err := os.Remove(req.Path)
	resp := map[string]interface{}{"success": err == nil}
	if err != nil {
		slog.Debug("Failed to delete file", "path", req.Path, "error", err)
		resp["error"] = err.Error()
	} else {
		slog.Debug("File deleted successfully", "path", req.Path)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
