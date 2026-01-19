package main

import (
	"context"
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

	sandboxSecret := os.Getenv("SANDBOX_SECRET")
	if sandboxSecret == "" {
		slog.Error("SANDBOX_SECRET environment variable not set")
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3030"
	}

	proxyPort := os.Getenv("PROXY_PORT")
	if proxyPort == "" {
		proxyPort = "3031"
	}

	srv := server.New(sandboxSecret)
	mux := srv.RegisterRoutes()

	// Start the main HTTP server
	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	slog.Info("Starting sandbox-executor", "version", Version, "port", port)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Start the TCP proxy server on user port
	slog.Info("Starting TCP proxy", "port", proxyPort)
	go func() {
		if err := srv.StartTCPProxy(proxyPort); err != nil {
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
