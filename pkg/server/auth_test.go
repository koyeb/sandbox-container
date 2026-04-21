package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newPoolTestServer(t *testing.T, secretPath string) (*Server, http.Handler) {
	t.Helper()

	srv, err := New(AuthConfig{
		Mode:       AuthModePool,
		SecretPath: secretPath,
	})
	if err != nil {
		t.Fatalf("failed to create pool test server: %v", err)
	}

	return srv, srv.RegisterRoutes()
}

func newAuthHeaderRequest(method, path, authHeader string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

func TestStaticAuthRejectsMissingBearerToken(t *testing.T) {
	_, mux := newTestServer(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, newAuthHeaderRequest(http.MethodGet, "/list_processes", ""))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestPoolAuthBootstrapsOnFirstAuthenticatedRequest(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "sandbox-secret")
	_, mux := newPoolTestServer(t, secretPath)

	first := httptest.NewRecorder()
	mux.ServeHTTP(first, newAuthHeaderRequest(http.MethodGet, "/list_processes", "Bearer pooled-secret"))

	if first.Code != http.StatusOK {
		t.Fatalf("expected bootstrap request to succeed, got %d", first.Code)
	}

	content, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("expected secret file to be written: %v", err)
	}
	if string(content) != "pooled-secret" {
		t.Fatalf("expected persisted secret to match bootstrap secret, got %q", string(content))
	}

	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("expected secret file stat to succeed: %v", err)
	}
	if info.Mode().Perm() != fs.FileMode(0o600) {
		t.Fatalf("expected secret file permissions 0600, got %o", info.Mode().Perm())
	}

	second := httptest.NewRecorder()
	mux.ServeHTTP(second, newAuthHeaderRequest(http.MethodGet, "/list_processes", "Bearer pooled-secret"))
	if second.Code != http.StatusOK {
		t.Fatalf("expected reused secret to be accepted, got %d", second.Code)
	}

	wrong := httptest.NewRecorder()
	mux.ServeHTTP(wrong, newAuthHeaderRequest(http.MethodGet, "/list_processes", "Bearer wrong-secret"))
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong secret to be rejected, got %d", wrong.Code)
	}
}

func TestPoolAuthRejectsMissingOrMalformedBearerBeforeBootstrap(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "sandbox-secret")
	_, mux := newPoolTestServer(t, secretPath)

	missing := httptest.NewRecorder()
	mux.ServeHTTP(missing, newAuthHeaderRequest(http.MethodGet, "/list_processes", ""))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("expected missing auth to be rejected, got %d", missing.Code)
	}

	malformed := httptest.NewRecorder()
	mux.ServeHTTP(malformed, newAuthHeaderRequest(http.MethodGet, "/list_processes", "Basic abc123"))
	if malformed.Code != http.StatusUnauthorized {
		t.Fatalf("expected malformed auth to be rejected, got %d", malformed.Code)
	}

	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Fatalf("expected secret file to remain absent, stat err=%v", err)
	}
}

func TestPoolAuthRestoresPersistedSecretOnRestart(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "sandbox-secret")
	_, mux := newPoolTestServer(t, secretPath)

	bootstrap := httptest.NewRecorder()
	mux.ServeHTTP(bootstrap, newAuthHeaderRequest(http.MethodGet, "/list_processes", "Bearer restart-secret"))
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("expected bootstrap request to succeed, got %d", bootstrap.Code)
	}

	_, restartedMux := newPoolTestServer(t, secretPath)

	reused := httptest.NewRecorder()
	restartedMux.ServeHTTP(reused, newAuthHeaderRequest(http.MethodGet, "/list_processes", "Bearer restart-secret"))
	if reused.Code != http.StatusOK {
		t.Fatalf("expected restored secret to be accepted, got %d", reused.Code)
	}

	wrong := httptest.NewRecorder()
	restartedMux.ServeHTTP(wrong, newAuthHeaderRequest(http.MethodGet, "/list_processes", "Bearer another-secret"))
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("expected different secret to be rejected after restart, got %d", wrong.Code)
	}
}

func TestPoolAuthConcurrentBootstrapOnlyPersistsOneSecret(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "sandbox-secret")
	_, mux := newPoolTestServer(t, secretPath)

	secrets := []string{
		"Bearer secret-one",
		"Bearer secret-two",
		"Bearer secret-three",
		"Bearer secret-four",
		"Bearer secret-five",
	}

	type result struct {
		code   int
		secret string
	}

	start := make(chan struct{})
	results := make(chan result, len(secrets))
	var wg sync.WaitGroup

	for _, secret := range secrets {
		wg.Add(1)
		go func(secret string) {
			defer wg.Done()
			<-start

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, newAuthHeaderRequest(http.MethodGet, "/list_processes", secret))
			results <- result{code: w.Code, secret: secret}
		}(secret)
	}

	close(start)
	wg.Wait()
	close(results)

	successes := 0
	var winningSecret string
	for result := range results {
		switch result.code {
		case http.StatusOK:
			successes++
			winningSecret = result.secret
		case http.StatusUnauthorized:
		default:
			t.Fatalf("unexpected status %d for %q", result.code, result.secret)
		}
	}

	if successes != 1 {
		t.Fatalf("expected exactly one bootstrap winner, got %d", successes)
	}

	content, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("expected secret file to exist: %v", err)
	}
	if "Bearer "+string(content) != winningSecret {
		t.Fatalf("expected persisted secret to match winner, got %q want %q", string(content), winningSecret)
	}
}

func TestPoolAuthFailsToStartWithInvalidSecretFile(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		secretPath := filepath.Join(t.TempDir(), "sandbox-secret")
		if err := os.WriteFile(secretPath, []byte("\n"), 0o600); err != nil {
			t.Fatalf("failed to create empty secret file: %v", err)
		}

		_, err := New(AuthConfig{
			Mode:       AuthModePool,
			SecretPath: secretPath,
		})
		if err == nil {
			t.Fatal("expected error when pool secret file is empty")
		}
	})

	t.Run("directory path", func(t *testing.T) {
		secretPath := filepath.Join(t.TempDir(), "sandbox-secret")
		if err := os.Mkdir(secretPath, 0o700); err != nil {
			t.Fatalf("failed to create secret path directory: %v", err)
		}

		_, err := New(AuthConfig{
			Mode:       AuthModePool,
			SecretPath: secretPath,
		})
		if err == nil {
			t.Fatal("expected error when pool secret path is unreadable as a file")
		}
	})
}
