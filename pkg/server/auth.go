package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type AuthMode string

const (
	AuthModeStatic AuthMode = "static"
	AuthModePool   AuthMode = "pool"

	DefaultPoolSecretPath = "/var/lib/sandbox-container/sandbox-secret"
)

type AuthConfig struct {
	Mode       AuthMode
	Secret     string
	SecretPath string
}

type authState struct {
	mu          sync.Mutex
	mode        AuthMode
	secret      string
	secretPath  string
	initialized bool
}

func newAuthState(config AuthConfig) (*authState, error) {
	mode := config.Mode
	if mode == "" {
		mode = AuthModeStatic
	}

	state := &authState{
		mode:       mode,
		secretPath: config.SecretPath,
	}

	switch mode {
	case AuthModeStatic:
		if config.Secret == "" {
			return nil, fmt.Errorf("SANDBOX_SECRET environment variable not set")
		}
		state.secret = config.Secret
		state.initialized = true
	case AuthModePool:
		if config.Secret != "" {
			return nil, fmt.Errorf("SANDBOX_SECRET cannot be set when SANDBOX_AUTH_MODE=pool")
		}
		if config.SecretPath == "" {
			return nil, fmt.Errorf("SANDBOX_SECRET_PATH must be set when SANDBOX_AUTH_MODE=pool")
		}
		if err := ensureSecretDir(config.SecretPath); err != nil {
			return nil, err
		}
		secret, ok, err := loadSecretFromDisk(config.SecretPath)
		if err != nil {
			return nil, err
		}
		if ok {
			state.secret = secret
			state.initialized = true
			slog.Info("Pool auth restored from disk", "secret_path", config.SecretPath)
		} else {
			slog.Info("Pool auth waiting for first authenticated request", "secret_path", config.SecretPath)
		}
	default:
		return nil, fmt.Errorf("unsupported SANDBOX_AUTH_MODE %q", mode)
	}

	return state, nil
}

func (a *authState) authorize(authHeader string) (authorized bool, bootstrapped bool, err error) {
	secret, ok := extractBearerSecret(authHeader)
	if !ok {
		return false, false, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.initialized {
		return secretsEqual(secret, a.secret), false, nil
	}

	if a.mode != AuthModePool {
		return false, false, nil
	}

	if err := a.persistSecretLocked(secret); err != nil {
		return false, false, err
	}

	a.secret = secret
	a.initialized = true
	slog.Info("Pool auth secret persisted from first request", "secret_path", a.secretPath)

	return true, true, nil
}

func extractBearerSecret(header string) (string, bool) {
	const prefix = "Bearer "

	if !strings.HasPrefix(header, prefix) {
		return "", false
	}

	secret := strings.TrimPrefix(header, prefix)
	if secret == "" {
		return "", false
	}

	return secret, true
}

func secretsEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func ensureSecretDir(secretPath string) error {
	dir := filepath.Dir(secretPath)
	if dir == "." {
		return nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create secret directory %q: %w", dir, err)
	}

	return nil
}

func loadSecretFromDisk(secretPath string) (string, bool, error) {
	content, err := os.ReadFile(secretPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to read secret file %q: %w", secretPath, err)
	}

	secret := strings.TrimSpace(string(content))
	if secret == "" {
		return "", false, fmt.Errorf("secret file %q is empty", secretPath)
	}

	return secret, true, nil
}

func (a *authState) persistSecretLocked(secret string) error {
	if err := ensureSecretDir(a.secretPath); err != nil {
		return err
	}

	dir := filepath.Dir(a.secretPath)
	tmpFile, err := os.CreateTemp(dir, ".sandbox-secret-*")
	if err != nil {
		return fmt.Errorf("failed to create temp secret file: %w", err)
	}

	tmpPath := tmpFile.Name()
	keepTemp := false
	defer func() {
		if keepTemp {
			return
		}
		_ = os.Remove(tmpPath)
	}()

	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to chmod temp secret file: %w", err)
	}

	if _, err := tmpFile.WriteString(secret); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write temp secret file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp secret file: %w", err)
	}

	if err := os.Rename(tmpPath, a.secretPath); err != nil {
		return fmt.Errorf("failed to persist secret file %q: %w", a.secretPath, err)
	}

	keepTemp = true

	if err := os.Chmod(a.secretPath, 0o600); err != nil {
		return fmt.Errorf("failed to chmod secret file %q: %w", a.secretPath, err)
	}

	return nil
}
