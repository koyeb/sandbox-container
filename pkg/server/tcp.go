package server

import (
	"fmt"
	"net"
	"sync"
)

// Connection wraps a net.Conn for easier handling
type Connection struct {
	net.Conn
}

// TCPListener manages a TCP listener
type TCPListener struct {
	port     string
	listener net.Listener
	mu       sync.Mutex
	stopChan chan struct{}
	wg       sync.WaitGroup
}

// NewTCPListener creates a new TCP listener
func NewTCPListener(port string) (*TCPListener, error) {
	return &TCPListener{
		port:     port,
		stopChan: make(chan struct{}),
	}, nil
}

// Start begins listening for TCP connections
func (l *TCPListener) Start(handler func(*Connection)) error {
	listener, err := net.Listen("tcp", ":"+l.port)
	if err != nil {
		return fmt.Errorf("failed to listen on port %s: %w", l.port, err)
	}

	l.mu.Lock()
	l.listener = listener
	l.mu.Unlock()

	go l.acceptLoop(handler)
	return nil
}

// acceptLoop handles incoming connections
func (l *TCPListener) acceptLoop(handler func(*Connection)) {
	for {
		select {
		case <-l.stopChan:
			return
		default:
		}

		conn, err := l.listener.Accept()
		if err != nil {
			select {
			case <-l.stopChan:
				return
			default:
				continue
			}
		}

		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			handler(&Connection{Conn: conn})
		}()
	}
}

// Stop closes the listener and waits for all connections to finish
func (l *TCPListener) Stop() {
	close(l.stopChan)

	l.mu.Lock()
	if l.listener != nil {
		l.listener.Close()
	}
	l.mu.Unlock()

	l.wg.Wait()
}

// DialTCP creates a TCP connection to the given address
func DialTCP(address string) (net.Conn, error) {
	return net.Dial("tcp", address)
}
