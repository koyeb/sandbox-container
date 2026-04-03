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

func newTestServer() (*Server, http.Handler) {
	srv := New("test-secret")
	return srv, srv.RegisterRoutes()
}

func newAuthRequest(method, path string, body []byte) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestRunHandlerLongOutput verifies that /run handles output lines with large payloads
func TestRunHandlerLongOutput(t *testing.T) {
	_, mux := newTestServer()

	longLine := strings.Repeat("a", 1024*1024*10)
	reqBody, _ := json.Marshal(RunRequest{Cmd: fmt.Sprintf("printf '%s\\n'", longLine)})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/run", reqBody))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !strings.Contains(resp.Stdout, longLine) {
		t.Errorf("stdout did not contain the expected long line (got %d chars)", len(resp.Stdout))
	}
}

// TestRunStreamingHandlerLongOutput verifies that /run_streaming handles large payloads
func TestRunStreamingHandlerLongOutput(t *testing.T) {
	_, mux := newTestServer()

	longLine := strings.Repeat("a", 1024*1024*10)
	reqBody, _ := json.Marshal(RunRequest{Cmd: fmt.Sprintf("printf '%s\\n'", longLine)})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthRequest(http.MethodPost, "/run_streaming", reqBody))

	body := w.Body.String()
	if !strings.Contains(body, longLine) {
		t.Errorf("SSE stream did not contain the expected long line (body length: %d)", len(body))
	}
}
