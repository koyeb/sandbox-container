package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
