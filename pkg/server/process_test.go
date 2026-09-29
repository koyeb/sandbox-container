package server

import (
	"testing"
	"time"
)

// waitForProcess waits for the process to exit and returns its final status,
// read under the lock that waitForCompletion writes it with.
func waitForProcess(t *testing.T, process *Process) ProcessStatus {
	t.Helper()
	select {
	case <-process.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("process %s did not exit", process.ID)
	}
	process.mu.RLock()
	defer process.mu.RUnlock()
	return process.Status
}

// waitForLogs polls the process logs until found returns true, since the output
// goroutines may still be appending when the process exits.
func waitForLogs(t *testing.T, pm *ProcessManager, id string, found func([]LogEntry) bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		logs, err := pm.GetProcessLogs(id)
		if err != nil {
			t.Fatalf("Failed to get logs: %v", err)
		}
		if found(logs) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProcessManager_StartProcess(t *testing.T) {
	pm := NewProcessManager()

	process, err := pm.StartProcess("echo 'Hello World'", "", nil)
	if err != nil {
		t.Fatalf("Failed to start process: %v", err)
	}

	if process.ID == "" {
		t.Error("Process ID should not be empty")
	}

	if process.PID == 0 {
		t.Error("Process PID should not be 0")
	}

	if status := waitForProcess(t, process); status != ProcessStatusCompleted {
		t.Errorf("Expected status Completed, got %s", status)
	}
}

func TestProcessManager_ListProcesses(t *testing.T) {
	pm := NewProcessManager()

	// Start a few processes
	_, err := pm.StartProcess("echo 'Test 1'", "", nil)
	if err != nil {
		t.Fatalf("Failed to start process 1: %v", err)
	}

	_, err = pm.StartProcess("echo 'Test 2'", "", nil)
	if err != nil {
		t.Fatalf("Failed to start process 2: %v", err)
	}

	processes := pm.ListProcesses()
	if len(processes) != 2 {
		t.Errorf("Expected 2 processes, got %d", len(processes))
	}
}

func TestProcessManager_GetProcess(t *testing.T) {
	pm := NewProcessManager()

	process, err := pm.StartProcess("sleep 1", "", nil)
	if err != nil {
		t.Fatalf("Failed to start process: %v", err)
	}

	retrieved, err := pm.GetProcess(process.ID)
	if err != nil {
		t.Fatalf("Failed to get process: %v", err)
	}

	if retrieved.ID != process.ID {
		t.Errorf("Expected ID %s, got %s", process.ID, retrieved.ID)
	}

	// Test non-existent process
	_, err = pm.GetProcess("non-existent-id")
	if err == nil {
		t.Error("Expected error for non-existent process")
	}
}

func TestProcessManager_KillProcess(t *testing.T) {
	pm := NewProcessManager()

	// Start a long-running process
	process, err := pm.StartProcess("sleep 10", "", nil)
	if err != nil {
		t.Fatalf("Failed to start process: %v", err)
	}

	// Give it a moment to start
	time.Sleep(100 * time.Millisecond)

	// Kill the process
	err = pm.KillProcess(process.ID)
	if err != nil {
		t.Fatalf("Failed to kill process: %v", err)
	}

	// Check status
	status := waitForProcess(t, process)
	if status != ProcessStatusKilled && status != ProcessStatusFailed {
		t.Errorf("Expected status Killed or Failed, got %s", status)
	}
}

func TestProcessManager_GetProcessLogs(t *testing.T) {
	pm := NewProcessManager()

	// Start a process that generates output
	process, err := pm.StartProcess("bash -c 'echo Line1; echo Line2; echo Error >&2'", "", nil)
	if err != nil {
		t.Fatalf("Failed to start process: %v", err)
	}

	// Check for both stdout and stderr entries
	hasStdout := false
	hasStderr := false
	waitForLogs(t, pm, process.ID, func(logs []LogEntry) bool {
		for _, entry := range logs {
			if entry.Stream == "stdout" {
				hasStdout = true
			}
			if entry.Stream == "stderr" {
				hasStderr = true
			}
		}
		return hasStdout && hasStderr
	})

	if !hasStdout {
		t.Error("Expected stdout entries")
	}
	if !hasStderr {
		t.Error("Expected stderr entries")
	}
}

func TestProcessManager_StreamProcessLogs(t *testing.T) {
	pm := NewProcessManager()

	// Start a process that generates output over time
	process, err := pm.StartProcess("bash -c 'for i in 1 2 3; do echo Line$i; sleep 0.1; done'", "", nil)
	if err != nil {
		t.Fatalf("Failed to start process: %v", err)
	}

	// Stream logs
	logChan, err := pm.StreamProcessLogs(process.ID)
	if err != nil {
		t.Fatalf("Failed to stream logs: %v", err)
	}

	logCount := 0
	timeout := time.After(2 * time.Second)

	for {
		select {
		case entry, ok := <-logChan:
			if !ok {
				// Channel closed, stream ended
				if logCount == 0 {
					t.Error("Expected at least one log entry")
				}
				return
			}
			logCount++
			if entry.Stream != "stdout" && entry.Stream != "stderr" {
				t.Errorf("Invalid stream type: %s", entry.Stream)
			}
		case <-timeout:
			t.Fatal("Test timeout waiting for logs")
		}
	}
}

func TestProcess_ToJSON(t *testing.T) {
	process := &Process{
		ID:        "test-id",
		PID:       12345,
		Status:    ProcessStatusRunning,
		Command:   "echo test",
		StartTime: time.Now(),
	}

	json := process.ToJSON()

	if json["id"] != "test-id" {
		t.Errorf("Expected id 'test-id', got %v", json["id"])
	}

	if json["pid"] != 12345 {
		t.Errorf("Expected pid 12345, got %v", json["pid"])
	}

	if json["status"] != ProcessStatusRunning {
		t.Errorf("Expected status Running, got %v", json["status"])
	}
}

func TestProcess_ToSummaryJSON(t *testing.T) {
	process := &Process{
		ID:      "test-id",
		PID:     12345,
		Status:  ProcessStatusRunning,
		Command: "echo test",
	}

	json := process.ToSummaryJSON()

	if len(json) != 4 {
		t.Errorf("Expected 4 fields in summary, got %d", len(json))
	}

	if json["id"] != "test-id" {
		t.Errorf("Expected id 'test-id', got %v", json["id"])
	}

	if json["pid"] != 12345 {
		t.Errorf("Expected pid 12345, got %v", json["pid"])
	}

	if json["status"] != ProcessStatusRunning {
		t.Errorf("Expected status Running, got %v", json["status"])
	}

	if json["command"] != "echo test" {
		t.Errorf("Expected command 'echo test', got %v", json["command"])
	}
}

func TestLogBuffer_Append(t *testing.T) {
	lb := NewLogBuffer(5)

	// Add 3 entries
	for i := 0; i < 3; i++ {
		lb.Append(LogEntry{
			Timestamp: time.Now(),
			Stream:    "stdout",
			Data:      "test",
		})
	}

	logs := lb.GetAll()
	if len(logs) != 3 {
		t.Errorf("Expected 3 logs, got %d", len(logs))
	}
}

func TestLogBuffer_MaxEntries(t *testing.T) {
	lb := NewLogBuffer(3)

	// Add 5 entries (more than max)
	for i := 0; i < 5; i++ {
		lb.Append(LogEntry{
			Timestamp: time.Now(),
			Stream:    "stdout",
			Data:      "test",
		})
	}

	logs := lb.GetAll()
	if len(logs) != 3 {
		t.Errorf("Expected 3 logs (max), got %d", len(logs))
	}
}

func TestProcessWithEnvironment(t *testing.T) {
	pm := NewProcessManager()

	env := map[string]string{
		"TEST_VAR": "test_value",
	}

	process, err := pm.StartProcess("bash -c 'echo $TEST_VAR'", "", env)
	if err != nil {
		t.Fatalf("Failed to start process: %v", err)
	}

	foundValue := waitForLogs(t, pm, process.ID, func(logs []LogEntry) bool {
		for _, entry := range logs {
			if entry.Data == "test_value" {
				return true
			}
		}
		return false
	})

	if !foundValue {
		t.Error("Expected to find environment variable value in output")
	}
}

func TestProcessWithWorkingDirectory(t *testing.T) {
	pm := NewProcessManager()

	// Use /tmp as working directory
	process, err := pm.StartProcess("pwd", "/tmp", nil)
	if err != nil {
		t.Fatalf("Failed to start process: %v", err)
	}

	foundTmp := waitForLogs(t, pm, process.ID, func(logs []LogEntry) bool {
		for _, entry := range logs {
			if entry.Data == "/tmp" || entry.Data == "/private/tmp" { // macOS uses /private/tmp
				return true
			}
		}
		return false
	})

	if !foundTmp {
		t.Error("Expected working directory to be /tmp")
	}
}
