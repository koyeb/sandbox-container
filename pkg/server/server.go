package server

import (
	"net/http"
)

type Server struct {
	sandboxSecret string
}

func New(sandboxSecret string) *Server {
	return &Server{
		sandboxSecret: sandboxSecret,
	}
}

func (s *Server) RegisterRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.healthHandler)
	mux.Handle("/run", s.authMiddleware(http.HandlerFunc(s.runHandler)))
	mux.Handle("/write_file", s.authMiddleware(http.HandlerFunc(s.writeFileHandler)))
	mux.Handle("/read_file", s.authMiddleware(http.HandlerFunc(s.readFileHandler)))
	mux.Handle("/delete_file", s.authMiddleware(http.HandlerFunc(s.deleteFileHandler)))
	mux.Handle("/delete_dir", s.authMiddleware(http.HandlerFunc(s.deleteDirHandler)))
	mux.Handle("/make_dir", s.authMiddleware(http.HandlerFunc(s.makeDirHandler)))
	mux.Handle("/list_dir", s.authMiddleware(http.HandlerFunc(s.listDirHandler)))
	return mux
}
