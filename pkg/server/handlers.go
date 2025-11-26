package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

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
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) runHandler(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
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
		http.Error(w, "Failed to get stdout", http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, "Failed to get stderr", http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, "Failed to start command", http.StatusInternalServerError)
		return
	}
	outBytes, _ := io.ReadAll(stdout)
	errBytes, _ := io.ReadAll(stderr)
	cmd.Wait()

	resp := RunResponse{
		Stdout: string(outBytes),
		Stderr: string(errBytes),
		Code:   cmd.ProcessState.ExitCode(),
	}
	if cmd.ProcessState.ExitCode() != 0 {
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

	process, err := s.processManager.StartProcess(req.Cmd, req.Cwd, req.Env)
	if err != nil {
		resp := StartProcessResponse{
			Error: err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(resp)
		return
	}

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

	processes := s.processManager.ListProcesses()

	processesData := make([]map[string]interface{}, len(processes))
	for i, p := range processes {
		processesData[i] = p.ToSummaryJSON()
	}

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

	err := s.processManager.KillProcess(req.ID)
	if err != nil {
		resp := KillProcessResponse{
			Success: false,
			Error:   err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(resp)
		return
	}

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

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	logChan, err := s.processManager.StreamProcessLogs(processID)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"error\": \"%s\"}\n\n", err.Error())
		flusher.Flush()
		return
	}

	// Stream logs as they arrive
	for entry := range logChan {
		data, _ := json.Marshal(entry)
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
		flusher.Flush()
	}

	// Send completion event
	fmt.Fprintf(w, "event: complete\ndata: {\"message\": \"stream ended\"}\n\n")
	flusher.Flush()
}

func (s *Server) runStreamingHandler(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	switch r.Method {
	case http.MethodPost:
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
	case http.MethodGet:
		req.Cmd = r.URL.Query().Get("cmd")
		if req.Cmd == "" {
			http.Error(w, "Command is required", http.StatusBadRequest)
			return
		}
		req.Cwd = r.URL.Query().Get("cwd")
		// envs can be passed as multiple query parameters: ?env=key=value
		envs := r.URL.Query()["env"]
		req.Env = make(map[string]string)
		for _, env := range envs {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) == 2 {
				req.Env[parts[0]] = parts[1]
			}
		}
	}

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

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
		fmt.Fprintf(w, "event: error\ndata: {\"error\": \"Failed to get stdout\"}\n\n")
		flusher.Flush()
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"error\": \"Failed to get stderr\"}\n\n")
		flusher.Flush()
		return
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"error\": \"Failed to start command\"}\n\n")
		flusher.Flush()
		return
	}

	// Stream stdout
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			data, _ := json.Marshal(map[string]string{"stream": "stdout", "data": line})
			fmt.Fprintf(w, "event: output\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}()

	// Stream stderr
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			data, _ := json.Marshal(map[string]string{"stream": "stderr", "data": line})
			fmt.Fprintf(w, "event: output\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}()

	// Wait for command to finish
	err = cmd.Wait()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	// Send completion event
	completeData, _ := json.Marshal(map[string]interface{}{
		"code":  exitCode,
		"error": err != nil,
	})
	fmt.Fprintf(w, "event: complete\ndata: %s\n\n", completeData)
	flusher.Flush()
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

	// Check if a port is already bound
	currentPort := s.tcpProxy.GetTargetPort()
	if currentPort != "" {
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

	resp := map[string]interface{}{
		"success": true,
		"message": "Port binding configured",
		"port":    req.Port,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) unbindPortHandler(w http.ResponseWriter, r *http.Request) {
	s.tcpProxy.ClearTargetPort()

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
	err := os.RemoveAll(req.Path)
	resp := map[string]interface{}{"success": err == nil}
	if err != nil {
		resp["error"] = err.Error()
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
	err := os.MkdirAll(req.Path, 0o755)
	resp := map[string]interface{}{"success": err == nil}
	if err != nil {
		resp["error"] = err.Error()
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
	entries, err := os.ReadDir(req.Path)
	resp := ListDirResponse{}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Entries = make([]string, len(entries))
		for i, entry := range entries {
			resp.Entries[i] = entry.Name()
		}
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
	err := os.WriteFile(req.Path, []byte(req.Content), 0o644)
	resp := map[string]interface{}{"success": err == nil}
	if err != nil {
		resp["error"] = err.Error()
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
	content, err := os.ReadFile(req.Path)
	resp := ReadFileResponse{}
	if err != nil {
		resp.Error = err.Error()
	} else {
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
	err := os.Remove(req.Path)
	resp := map[string]interface{}{"success": err == nil}
	if err != nil {
		resp["error"] = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
