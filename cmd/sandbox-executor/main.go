package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/koyeb/sandbox-container/pkg/server"
)

func main() {
	sandboxSecret := os.Getenv("SANDBOX_SECRET")
	if sandboxSecret == "" {
		log.Fatal("SANDBOX_SECRET environment variable not set")
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

	log.Printf("Starting sandbox-executor on port %s", port)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Start the TCP proxy server on user port
	log.Printf("Starting TCP proxy on port %s", proxyPort)
	go func() {
		if err := srv.StartTCPProxy(proxyPort); err != nil {
			log.Fatalf("TCP proxy failed to start: %v", err)
		}
	}()

	// Wait for interrupt signal for graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down servers...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	srv.StopTCPProxy()
	log.Println("Servers stopped")
	}
