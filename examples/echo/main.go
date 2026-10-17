// Command echo is a WebSocket echo server: it sends every message back to
// the client that sent it, with the same message type.
//
// Small as it is, it has the shape of a production server: one read loop per
// connection, an incoming size limit, a bounded write, and a graceful
// shutdown. On an interrupt or SIGTERM the server stops accepting connections
// and ends each open one with [websocket.Conn.Shutdown] and
// [websocket.StatusGoingAway], so clients learn why they were disconnected
// instead of seeing a connection reset. Hijacked connections are invisible to
// [http.Server.Shutdown], so the server tracks them itself.
//
// Run it, then connect with any WebSocket client, such as the client example:
//
//	go run ./examples/echo
//	go run ./examples/client ws://localhost:8080/ws
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/picatz/websocket"
)

const (
	// maxMessage bounds each incoming message.
	maxMessage = 64 << 10
	// writeWait bounds each echoed message.
	writeWait = 10 * time.Second
	// shutdownWait bounds the closing handshake with each client, and the
	// wait for HTTP handlers, when the server shuts down.
	shutdownWait = 5 * time.Second
)

// server echoes messages on every connection until ctx ends.
type server struct {
	// ctx is done once the server starts shutting down.
	ctx      context.Context
	upgrader websocket.Upgrader
	// conns counts handlers, including those serving a WebSocket.
	conns sync.WaitGroup
}

func newServer(ctx context.Context) *server {
	return &server{ctx: ctx, upgrader: websocket.Upgrader{MaxMessageSize: maxMessage}}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Count the handler before Upgrade hijacks the connection. Until then
	// http.Server.Shutdown waits for it, so once Shutdown returns no handler
	// can still call Add, and conns.Wait is safe.
	s.conns.Add(1)
	defer s.conns.Done()

	conn, err := s.upgrader.Upgrade(w, r)
	if err != nil {
		// Upgrade has already written the HTTP error response.
		log.Printf("%s: upgrade: %v", r.RemoteAddr, err)
		return
	}
	defer conn.Close()

	// When the server shuts down, end the connection with the closing
	// handshake. The read loop below then receives the client's reply as a
	// *websocket.CloseError and returns.
	shutdown := make(chan error, 1)
	stop := context.AfterFunc(s.ctx, func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		defer cancel()
		shutdown <- conn.Shutdown(ctx, websocket.StatusGoingAway, "server shutting down")
	})
	defer func() {
		if !stop() {
			<-shutdown // Let the handshake finish before reporting the handler done.
		}
	}()

	for {
		messageType, message, err := conn.ReadMessage(r.Context())
		if err != nil {
			// A *CloseError is the client leaving, and net.ErrClosed is the
			// server's own Shutdown; anything else is worth a log line.
			if websocket.CloseStatus(err) == -1 && !errors.Is(err, net.ErrClosed) {
				log.Printf("%s: read: %v", r.RemoteAddr, err)
			}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), writeWait)
		err = conn.WriteMessage(ctx, messageType, message)
		cancel()
		if err != nil {
			return
		}
	}
}

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := newServer(ctx)
	mux := http.NewServeMux()
	mux.Handle("/ws", s)
	srv := &http.Server{Addr: *addr, Handler: mux}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Printf("echo server listening on ws://%s/ws", *addr)

	select {
	case err := <-serveErr:
		log.Fatal(err)
	case <-ctx.Done():
	}
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Print(err)
	}
	s.conns.Wait()
}
