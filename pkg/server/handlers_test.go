package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()

	srv, err := New(AuthConfig{
		Mode:   AuthModeStatic,
		Secret: "test-secret",
	})
	if err != nil {
		t.Fatalf("failed to create test server: %v", err)
	}

	return srv, srv.RegisterRoutes()
}

func newAuthRequest(method, path string, body []byte) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestRunHandlerLongOutput verifies that /run handles output lines with large payloads.
// Uses a pipeline to generate large output without hitting ARG_MAX limits.
func TestRunHandlerLongOutput(t *testing.T) {
	_, mux := newTestServer(t)

	const size = 1024 * 1024 * 10 // 10MB
	reqBody, _ := json.Marshal(RunRequest{Cmd: fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'a'", size)})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/run", reqBody))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Stdout) != size {
		t.Errorf("expected %d bytes of stdout, got %d", size, len(resp.Stdout))
	}
}

// TestRunStreamingHandlerLongOutput verifies that /run_streaming handles large payloads
// without hanging. Before the fix, the bufio.Scanner would stop reading after 64KB,
// fill the pipe buffer, and block cmd.Wait() indefinitely.
func TestRunStreamingHandlerLongOutput(t *testing.T) {
	_, mux := newTestServer(t)

	const size = 1024 * 1024 * 10 // 10MB
	reqBody, _ := json.Marshal(RunRequest{Cmd: fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'a'", size)})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/run_streaming", reqBody))

	// Parse SSE events and sum up stdout data lengths.
	total := 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]string
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["stream"] == "stdout" {
			total += len(event["data"])
		}
	}
	if total != size {
		t.Errorf("expected %d bytes of stdout data in SSE stream, got %d", size, total)
	}
}

func TestRunStreamingHandlerCarriageReturnProgress(t *testing.T) {
	_, mux := newTestServer(t)

	reqBody, _ := json.Marshal(RunRequest{Cmd: "printf 'Cloning into repo...\\rReceiving objects: 10%%\\rReceiving objects: 100%%\\n' >&2"})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/run_streaming", reqBody))

	want := []string{
		"Cloning into repo...",
		"Receiving objects: 10%",
		"Receiving objects: 100%",
	}
	got := make([]string, 0, len(want))
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]string
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["stream"] == "stderr" {
			got = append(got, event["data"])
		}
	}

	if len(got) != len(want) {
		t.Fatalf("expected stderr events %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stderr event %d: expected %q, got %q", i, want[i], got[i])
		}
	}
}

// streamingEvents runs cmd through /run_streaming and returns the data of its
// output events for the given stream, and its complete event.
func streamingEvents(t *testing.T, mux http.Handler, cmd, stream string) ([]string, map[string]any) {
	t.Helper()

	reqBody, _ := json.Marshal(RunRequest{Cmd: cmd})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/run_streaming", reqBody))

	var lines []string
	var complete map[string]any
	event := ""
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			event = name
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		switch event {
		case "output":
			var output map[string]string
			if json.Unmarshal([]byte(data), &output) == nil && output["stream"] == stream {
				lines = append(lines, output["data"])
			}
		case "complete":
			if err := json.Unmarshal([]byte(data), &complete); err != nil {
				t.Fatalf("failed to decode complete event %q: %v", data, err)
			}
		}
	}
	if complete == nil {
		t.Fatalf("no complete event in %q", w.Body.String())
	}
	return lines, complete
}

// TestRunStreamingHandlerCompleteOutput verifies that no output is lost when the
// command exits while output is still buffered in the pipe. Many short lines make the
// handler (one SSE event per line) slower than the command.
func TestRunStreamingHandlerCompleteOutput(t *testing.T) {
	_, mux := newTestServer(t)

	const n = 200000
	lines, complete := streamingEvents(t, mux, fmt.Sprintf("seq 1 %d", n), "stdout")

	if len(lines) != n {
		t.Fatalf("expected %d stdout events, got %d (last %q)", n, len(lines), lines[len(lines)-1])
	}
	if last := lines[len(lines)-1]; last != strconv.Itoa(n) {
		t.Fatalf("expected last stdout event %q, got %q", strconv.Itoa(n), last)
	}
	if complete["code"] != float64(0) || complete["error"] != false {
		t.Fatalf("expected complete {code:0 error:false}, got %v", complete)
	}
}

func TestRunStreamingHandlerBlankLines(t *testing.T) {
	_, mux := newTestServer(t)

	lines, _ := streamingEvents(t, mux, "printf 'a\\n\\nb\\r\\n\\r\\nc\\rd\\r\\re\\n'", "stdout")

	want := []string{"a", "", "b", "", "c", "d", "e"}
	if !slices.Equal(lines, want) {
		t.Fatalf("expected stdout events %q, got %q", want, lines)
	}
}

// TestRunStreamingHandlerKillsDescendantKeepingPipeOpen verifies that when a
// background child still holds the output after the command exits, the stream ends
// after the wait delay and the child is killed.
func TestRunStreamingHandlerKillsDescendantKeepingPipeOpen(t *testing.T) {
	const waitDelay = 500 * time.Millisecond
	oldWaitDelay := streamingCommandWaitDelay
	streamingCommandWaitDelay = waitDelay
	t.Cleanup(func() { streamingCommandWaitDelay = oldWaitDelay })

	_, mux := newTestServer(t)
	pidFile := filepath.Join(t.TempDir(), "pid")

	start := time.Now()
	done := make(chan struct{})
	var lines []string
	var complete map[string]any
	go func() {
		defer close(done)
		lines, complete = streamingEvents(t, mux, fmt.Sprintf("echo done; sleep 60 & echo $! > %s", pidFile), "stdout")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streaming handler did not complete after command exited")
	}
	elapsed := time.Since(start)

	if elapsed < waitDelay {
		t.Fatalf("expected the stream to last at least the wait delay %s, ended after %s", waitDelay, elapsed)
	}
	if !slices.Equal(lines, []string{"done"}) {
		t.Fatalf("expected stdout events [done], got %q", lines)
	}
	if complete["code"] != float64(0) || complete["error"] != false {
		t.Fatalf("expected complete {code:0 error:false}, got %v", complete)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("failed to read background pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("invalid background pid %q: %v", pidBytes, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processRunning(pid) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("background child %d still running after the stream ended", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processRunning reports whether pid exists and is not a zombie (an orphan whose
// reaper, e.g. PID 1 in a container, doesn't wait for it).
func processRunning(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// Format: pid (comm) state ...
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return len(fields) == 0 || fields[0] != "Z"
}

func TestStartProcessInvalidCwd(t *testing.T) {
	_, mux := newTestServer(t)

	reqBody, _ := json.Marshal(map[string]string{
		"cmd": "id",
		"cwd": "/invalid/path",
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/start_process", reqBody))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRunInvalidCwd(t *testing.T) {
	_, mux := newTestServer(t)

	reqBody, _ := json.Marshal(map[string]string{
		"cmd": "id",
		"cwd": "/invalid/path",
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/run", reqBody))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRunStreamingInvalidCwd(t *testing.T) {
	_, mux := newTestServer(t)

	reqBody, _ := json.Marshal(map[string]string{
		"cmd": "id",
		"cwd": "/invalid/path",
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/run_streaming", reqBody))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}
}
