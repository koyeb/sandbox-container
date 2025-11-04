package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type Server struct {
	sandboxSecret string
	tcpProxy      *TCPProxy
}

func New(sandboxSecret string) *Server {
	return &Server{
		sandboxSecret: sandboxSecret,
		tcpProxy:      NewTCPProxy(),
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
	mux.Handle("/bind_port", s.authMiddleware(http.HandlerFunc(s.bindPortHandler)))
	mux.Handle("/unbind_port", s.authMiddleware(http.HandlerFunc(s.unbindPortHandler)))
	return mux
}

// TCPProxy handles TCP forwarding to a configured target port
type TCPProxy struct {
	mu         sync.RWMutex
	targetPort string
	listener   *TCPListener
}

func NewTCPProxy() *TCPProxy {
	return &TCPProxy{}
}

func (p *TCPProxy) SetTargetPort(port string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targetPort = port
}

func (p *TCPProxy) GetTargetPort() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.targetPort
}

func (p *TCPProxy) ClearTargetPort() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targetPort = ""
}

func (p *TCPProxy) SetListener(listener *TCPListener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listener = listener
}

func (p *TCPProxy) GetListener() *TCPListener {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.listener
}

func (s *Server) StartTCPProxy(port string) error {
	listener, err := NewTCPListener(port)
	if err != nil {
		return fmt.Errorf("failed to create TCP listener: %w", err)
	}

	s.tcpProxy.SetListener(listener)

	return listener.Start(func(conn *Connection) {
		defer conn.Close()

		targetPort := s.tcpProxy.GetTargetPort()
		if targetPort == "" {
			// No target port configured - accept connection and wait briefly
			// This allows health checks to succeed
			buf := make([]byte, 1)
			conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			conn.Read(buf)
			return
		}

		// Connect to target port
		targetConn, err := DialTCP("localhost:" + targetPort)
		if err != nil {
			log.Printf("Failed to connect to target port %s: %v", targetPort, err)
			return
		}
		defer targetConn.Close()

		// Bidirectional copy
		done := make(chan error, 2)

		go func() {
			_, err := io.Copy(targetConn, conn)
			done <- err
		}()

		go func() {
			_, err := io.Copy(conn, targetConn)
			done <- err
		}()

		// Wait for either direction to complete
		<-done
	})
}

func (s *Server) StopTCPProxy() {
	if listener := s.tcpProxy.GetListener(); listener != nil {
		listener.Stop()
	}
}
