package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/koyeb/sandbox-container/pkg/server"
)

// Version is set via ldflags during build
var Version = "dev"

// LevelTrace is a custom log level below DEBUG for very verbose logging
const LevelTrace = slog.Level(-8)

type runtimeConfig struct {
	Port      string
	ProxyPort string
	Auth      server.AuthConfig
}

func main() {
	// Configure logger based on LOG_LEVEL environment variable
	logLevel := os.Getenv("LOG_LEVEL")
	var level slog.Level
	switch strings.ToUpper(logLevel) {
	case "TRACE":
		level = LevelTrace
	case "DEBUG":
		level = slog.LevelDebug
	case "INFO", "":
		level = slog.LevelInfo
	case "WARN":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	// Create a custom handler that properly formats the TRACE level
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				level := a.Value.Any().(slog.Level)
				if level == LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
			}
			return a
		},
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	config, err := loadConfigFromEnv()
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	srv, err := server.New(config.Auth)
	if err != nil {
		slog.Error("Failed to initialize server", "error", err)
		os.Exit(1)
	}
	mux := srv.RegisterRoutes()

	// Start the main HTTP server
	httpServer := &http.Server{
		Addr:    ":" + config.Port,
		Handler: mux,
	}

	slog.Info("Starting sandbox-executor", "version", Version, "port", config.Port, "auth_mode", config.Auth.Mode)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Start the TCP proxy server on user port
	slog.Info("Starting TCP proxy", "port", config.ProxyPort)
	go func() {
		if err := srv.StartTCPProxy(config.ProxyPort); err != nil {
			slog.Error("TCP proxy failed to start", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal for graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("Shutting down servers...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	srv.StopTCPProxy()
	slog.Info("Servers stopped")
}

func loadConfigFromEnv() (runtimeConfig, error) {
	config := runtimeConfig{
		Port:      getenvDefault("PORT", "3030"),
		ProxyPort: getenvDefault("PROXY_PORT", "3031"),
		Auth: server.AuthConfig{
			Mode:       server.AuthMode(strings.ToLower(os.Getenv("SANDBOX_AUTH_MODE"))),
			Secret:     os.Getenv("SANDBOX_SECRET"),
			SecretPath: os.Getenv("SANDBOX_SECRET_PATH"),
		},
	}

	if config.Auth.Mode == "" {
		config.Auth.Mode = server.AuthModeStatic
	}

	switch config.Auth.Mode {
	case server.AuthModeStatic:
		if config.Auth.Secret == "" {
			return runtimeConfig{}, fmt.Errorf("SANDBOX_SECRET environment variable not set")
		}
	case server.AuthModePool:
		if config.Auth.Secret != "" {
			return runtimeConfig{}, fmt.Errorf("SANDBOX_SECRET cannot be set when SANDBOX_AUTH_MODE=pool")
		}
		if config.Auth.SecretPath == "" {
			config.Auth.SecretPath = server.DefaultPoolSecretPath
		}
	default:
		return runtimeConfig{}, fmt.Errorf("unsupported SANDBOX_AUTH_MODE %q", config.Auth.Mode)
	}

	return config, nil
}

func getenvDefault(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}
