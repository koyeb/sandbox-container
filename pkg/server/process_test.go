package server

import (
	"testing"
	"time"
)

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

	if process.Status != ProcessStatusRunning {
		t.Errorf("Expected status Running, got %s", process.Status)
	}

	// Wait for process to complete
	time.Sleep(100 * time.Millisecond)
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

	// Wait for status to update
	time.Sleep(100 * time.Millisecond)

	// Check status
	retrieved, _ := pm.GetProcess(process.ID)
	if retrieved.Status != ProcessStatusKilled && retrieved.Status != ProcessStatusFailed {
		t.Errorf("Expected status Killed or Failed, got %s", retrieved.Status)
	}
}

func TestProcessManager_GetProcessLogs(t *testing.T) {
	pm := NewProcessManager()

	// Start a process that generates output
	process, err := pm.StartProcess("bash -c 'echo Line1; echo Line2; echo Error >&2'", "", nil)
	if err != nil {
		t.Fatalf("Failed to start process: %v", err)
	}

	// Wait for process to complete
	time.Sleep(200 * time.Millisecond)

	// Get logs
	logs, err := pm.GetProcessLogs(process.ID)
	if err != nil {
		t.Fatalf("Failed to get logs: %v", err)
	}

	if len(logs) == 0 {
		t.Error("Expected some log entries")
	}

	// Check for both stdout and stderr entries
	hasStdout := false
	hasStderr := false
	for _, entry := range logs {
		if entry.Stream == "stdout" {
			hasStdout = true
		}
		if entry.Stream == "stderr" {
			hasStderr = true
		}
	}

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

	// Wait for process to complete
	time.Sleep(200 * time.Millisecond)

	logs, _ := pm.GetProcessLogs(process.ID)
	
	foundValue := false
	for _, entry := range logs {
		if entry.Data == "test_value" {
			foundValue = true
			break
		}
	}

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

	// Wait for process to complete
	time.Sleep(200 * time.Millisecond)

	logs, _ := pm.GetProcessLogs(process.ID)
	
	foundTmp := false
	for _, entry := range logs {
		if entry.Data == "/tmp" || entry.Data == "/private/tmp" { // macOS uses /private/tmp
			foundTmp = true
			break
		}
	}

	if !foundTmp {
		t.Error("Expected working directory to be /tmp")
	}
}
