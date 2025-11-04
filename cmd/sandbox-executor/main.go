package main

import (
	"log"
	"net/http"
	"os"

	"github.com/koyeb/sandbox-container/pkg/server"
)

func main() {
	sandboxSecret := os.Getenv("SANDBOX_SECRET")
	if sandboxSecret == "" {
		log.Fatal("SANDBOX_SECRET environment variable not set")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	srv := server.New(sandboxSecret)
	mux := srv.RegisterRoutes()

	log.Printf("Starting sandbox-executor on port %s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
