package main

import (
	"testing"

	"github.com/koyeb/sandbox-container/pkg/server"
)

func TestLoadConfigFromEnvStaticMode(t *testing.T) {
	t.Setenv("SANDBOX_SECRET", "static-secret")
	t.Setenv("SANDBOX_AUTH_MODE", "")
	t.Setenv("SANDBOX_SECRET_PATH", "")
	t.Setenv("PORT", "")
	t.Setenv("PROXY_PORT", "")

	config, err := loadConfigFromEnv()
	if err != nil {
		t.Fatalf("expected static config to load: %v", err)
	}

	if config.Auth.Mode != server.AuthModeStatic {
		t.Fatalf("expected static auth mode, got %q", config.Auth.Mode)
	}
	if config.Auth.Secret != "static-secret" {
		t.Fatalf("expected static secret to be preserved, got %q", config.Auth.Secret)
	}
	if config.Port != "3030" || config.ProxyPort != "3031" {
		t.Fatalf("expected default ports, got port=%q proxy_port=%q", config.Port, config.ProxyPort)
	}
}

func TestLoadConfigFromEnvPoolModeDefaultsSecretPath(t *testing.T) {
	t.Setenv("SANDBOX_AUTH_MODE", "pool")
	t.Setenv("SANDBOX_SECRET", "")
	t.Setenv("SANDBOX_SECRET_PATH", "")

	config, err := loadConfigFromEnv()
	if err != nil {
		t.Fatalf("expected pool config to load: %v", err)
	}

	if config.Auth.Mode != server.AuthModePool {
		t.Fatalf("expected pool auth mode, got %q", config.Auth.Mode)
	}
	if config.Auth.SecretPath != server.DefaultPoolSecretPath {
		t.Fatalf("expected default secret path %q, got %q", server.DefaultPoolSecretPath, config.Auth.SecretPath)
	}
}

func TestLoadConfigFromEnvRejectsConflictingPoolSecret(t *testing.T) {
	t.Setenv("SANDBOX_AUTH_MODE", "pool")
	t.Setenv("SANDBOX_SECRET", "unexpected")

	_, err := loadConfigFromEnv()
	if err == nil {
		t.Fatal("expected pool config with SANDBOX_SECRET to fail")
	}
}

func TestLoadConfigFromEnvRejectsInvalidMode(t *testing.T) {
	t.Setenv("SANDBOX_AUTH_MODE", "surprise")
	t.Setenv("SANDBOX_SECRET", "")

	_, err := loadConfigFromEnv()
	if err == nil {
		t.Fatal("expected invalid auth mode to fail")
	}
}

func TestExtractCustomerCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "no separator",
			args: []string{"sandbox-executor"},
			want: nil,
		},
		{
			name: "with command after separator",
			args: []string{"sandbox-executor", "--", "/bin/sh", "-c", "my cmd"},
			want: []string{"/bin/sh", "-c", "my cmd"},
		},
		{
			name: "separator with no command",
			args: []string{"sandbox-executor", "--"},
			want: []string{},
		},
		{
			name: "separator with single command",
			args: []string{"sandbox-executor", "--", "echo"},
			want: []string{"echo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCustomerCommand(tt.args)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("arg %d: expected %q, got %q", i, tt.want[i], got[i])
				}
			}
		})
	}
}
