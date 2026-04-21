package server

import (
	"log/slog"
	"net/http"

	"github.com/koyeb/sandbox-container/pkg/logger"
)

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Trace("Auth check", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)

		authorized, bootstrapped, err := s.auth.authorize(r.Header.Get("Authorization"))
		if err != nil {
			slog.Error("Auth check failed", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		if !authorized {
			logger.Trace("Unauthorized request", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if bootstrapped {
			slog.Info("Pool auth bootstrapped", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
		}

		logger.Trace("Authorized request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
