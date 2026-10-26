// Package testnats runs an in-process NATS server for tests, so CI needs no
// NATS service.
package testnats

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// Server is a NATS server on a random local port, shut down at cleanup.
type Server struct {
	*server.Server
}

func New(t *testing.T) *Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(s.Shutdown)
	return &Server{s}
}

// Connect opens a client connection, closed at cleanup.
func (s *Server) Connect(t *testing.T) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}
