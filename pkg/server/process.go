package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ProcessStatus represents the status of a background process
type ProcessStatus string

const (
	ProcessStatusRunning  ProcessStatus = "running"
	ProcessStatusCompleted ProcessStatus = "completed"
	ProcessStatusFailed   ProcessStatus = "failed"
	ProcessStatusKilled   ProcessStatus = "killed"
)

// Process represents a background process
type Process struct {
	ID        string        `json:"id"`
	PID       int           `json:"pid"`
	Status    ProcessStatus `json:"status"`
	Command   string        `json:"command"`
	Cwd       string        `json:"cwd,omitempty"`
	StartTime time.Time     `json:"start_time"`
	EndTime   *time.Time    `json:"end_time,omitempty"`
	ExitCode  *int          `json:"exit_code,omitempty"`

	// Internal fields
	cmd       *exec.Cmd
	stdout    *LogBuffer
	stderr    *LogBuffer
	mu        sync.RWMutex
	logsMu    sync.RWMutex
	done      chan struct{}
	observers []chan LogEntry
}

// LogEntry represents a single log line
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"` // "stdout" or "stderr"
	Data      string    `json:"data"`
}

// LogBuffer stores process logs in memory with a maximum size
type LogBuffer struct {
	entries    []LogEntry
	mu         sync.RWMutex
	maxEntries int
}

func NewLogBuffer(maxEntries int) *LogBuffer {
	return &LogBuffer{
		entries:    make([]LogEntry, 0),
		maxEntries: maxEntries,
	}
}

func (lb *LogBuffer) Append(entry LogEntry) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.entries = append(lb.entries, entry)
	
	// Keep only the last maxEntries
	if len(lb.entries) > lb.maxEntries {
		lb.entries = lb.entries[len(lb.entries)-lb.maxEntries:]
	}
}

func (lb *LogBuffer) GetAll() []LogEntry {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	// Return a copy to avoid concurrent access issues
	result := make([]LogEntry, len(lb.entries))
	copy(result, lb.entries)
	return result
}

// ProcessManager manages background processes
type ProcessManager struct {
	processes map[string]*Process
	mu        sync.RWMutex
}

func NewProcessManager() *ProcessManager {
	return &ProcessManager{
		processes: make(map[string]*Process),
	}
}

// StartProcess starts a new background process
func (pm *ProcessManager) StartProcess(command, cwd string, env map[string]string) (*Process, error) {
	id := uuid.New().String()

	cmd := exec.Command("sh", "-c", command)
	
	if cwd != "" {
		cmd.Dir = cwd
	}

	if len(env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}

	process := &Process{
		ID:        id,
		Status:    ProcessStatusRunning,
		Command:   command,
		Cwd:       cwd,
		StartTime: time.Now(),
		cmd:       cmd,
		stdout:    NewLogBuffer(10000), // Store up to 10k log lines
		stderr:    NewLogBuffer(10000),
		done:      make(chan struct{}),
		observers: make([]chan LogEntry, 0),
	}

	// Get pipes for stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	process.PID = cmd.Process.Pid

	// Register the process
	pm.mu.Lock()
	pm.processes[id] = process
	pm.mu.Unlock()

	// Start goroutines to capture stdout and stderr
	go pm.captureOutput(process, stdoutPipe, "stdout")
	go pm.captureOutput(process, stderrPipe, "stderr")

	// Wait for process completion in background
	go pm.waitForCompletion(process)

	return process, nil
}

// captureOutput captures output from a pipe and stores it in the log buffer
func (pm *ProcessManager) captureOutput(process *Process, pipe io.Reader, stream string) {
	scanner := bufio.NewScanner(pipe)
	
	// Increase buffer size for long lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		entry := LogEntry{
			Timestamp: time.Now(),
			Stream:    stream,
			Data:      line,
		}

		// Store in appropriate buffer
		if stream == "stdout" {
			process.stdout.Append(entry)
		} else {
			process.stderr.Append(entry)
		}

		// Notify observers
		process.logsMu.RLock()
		for _, observer := range process.observers {
			select {
			case observer <- entry:
			default:
				// Don't block if observer is slow
			}
		}
		process.logsMu.RUnlock()
	}
}

// waitForCompletion waits for the process to complete and updates its status
func (pm *ProcessManager) waitForCompletion(process *Process) {
	err := process.cmd.Wait()
	
	process.mu.Lock()
	defer process.mu.Unlock()

	now := time.Now()
	process.EndTime = &now

	if err != nil {
		if process.cmd.ProcessState.ExitCode() == -1 {
			// Process was killed
			process.Status = ProcessStatusKilled
		} else {
			process.Status = ProcessStatusFailed
		}
	} else {
		process.Status = ProcessStatusCompleted
	}

	exitCode := process.cmd.ProcessState.ExitCode()
	process.ExitCode = &exitCode

	close(process.done)
}

// GetProcess retrieves a process by ID
func (pm *ProcessManager) GetProcess(id string) (*Process, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	process, exists := pm.processes[id]
	if !exists {
		return nil, fmt.Errorf("process not found: %s", id)
	}

	return process, nil
}

// ListProcesses returns all processes
func (pm *ProcessManager) ListProcesses() []*Process {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	processes := make([]*Process, 0, len(pm.processes))
	for _, p := range pm.processes {
		processes = append(processes, p)
	}

	return processes
}

// KillProcess kills a process by ID
func (pm *ProcessManager) KillProcess(id string) error {
	process, err := pm.GetProcess(id)
	if err != nil {
		return err
	}

	process.mu.RLock()
	cmd := process.cmd
	status := process.Status
	process.mu.RUnlock()

	if status != ProcessStatusRunning {
		return fmt.Errorf("process is not running (status: %s)", status)
	}

	if cmd.Process == nil {
		return fmt.Errorf("process has no PID")
	}

	return cmd.Process.Kill()
}

// GetProcessLogs returns all logs for a process
func (pm *ProcessManager) GetProcessLogs(id string) ([]LogEntry, error) {
	process, err := pm.GetProcess(id)
	if err != nil {
		return nil, err
	}

	// Merge stdout and stderr logs
	stdoutLogs := process.stdout.GetAll()
	stderrLogs := process.stderr.GetAll()

	allLogs := make([]LogEntry, 0, len(stdoutLogs)+len(stderrLogs))
	allLogs = append(allLogs, stdoutLogs...)
	allLogs = append(allLogs, stderrLogs...)

	// Sort by timestamp
	return allLogs, nil
}

// StreamProcessLogs creates a channel that receives new log entries
func (pm *ProcessManager) StreamProcessLogs(id string) (<-chan LogEntry, error) {
	process, err := pm.GetProcess(id)
	if err != nil {
		return nil, err
	}

	logChan := make(chan LogEntry, 100)

	process.logsMu.Lock()
	process.observers = append(process.observers, logChan)
	process.logsMu.Unlock()

	// Send existing logs first
	existingLogs, _ := pm.GetProcessLogs(id)
	go func() {
		for _, entry := range existingLogs {
			logChan <- entry
		}
	}()

	// Close channel when process is done
	go func() {
		<-process.done
		time.Sleep(100 * time.Millisecond) // Give time for final logs
		close(logChan)
	}()

	return logChan, nil
}

// ToJSON returns a JSON-serializable representation of the process
func (p *Process) ToJSON() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := map[string]interface{}{
		"id":         p.ID,
		"pid":        p.PID,
		"status":     p.Status,
		"command":    p.Command,
		"start_time": p.StartTime,
	}

	if p.Cwd != "" {
		result["cwd"] = p.Cwd
	}

	if p.EndTime != nil {
		result["end_time"] = p.EndTime
	}

	if p.ExitCode != nil {
		result["exit_code"] = *p.ExitCode
	}

	return result
}

// ToSummaryJSON returns a minimal JSON representation for list views
func (p *Process) ToSummaryJSON() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return map[string]interface{}{
		"id":     p.ID,
		"pid":    p.PID,
		"status": p.Status,
	}
}
