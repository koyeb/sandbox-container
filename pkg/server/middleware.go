package server

import (
	"net/http"

	"github.com/koyeb/sandbox-container/pkg/logger"
)

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Trace("Auth check", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)

		secret := r.Header.Get("Authorization")
		if secret != "Bearer "+s.sandboxSecret {
			logger.Trace("Unauthorized request", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		logger.Trace("Authorized request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
