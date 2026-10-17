// Command tunnel forwards TCP connections over WebSocket, for reaching a
// service through infrastructure that only passes HTTP, such as a load
// balancer or a corporate proxy.
//
// The server accepts WebSocket connections and connects each one to a fixed
// TCP target. The client listens on a local TCP address and carries each
// accepted connection to the server over its own WebSocket. Both ends use
// [websocket.NetConn], which turns a WebSocket into a net.Conn, so the
// forwarding itself is io.Copy in each direction.
//
// For example, to reach an SSH server through the tunnel:
//
//	go run ./examples/tunnel server -addr localhost:8080 -target localhost:22
//	go run ./examples/tunnel client -listen localhost:2222 ws://localhost:8080/tunnel
//	ssh -p 2222 localhost
//
// When either side of a forwarded connection ends, both are closed: a
// WebSocket has no equivalent of a TCP half-close. A peer that aborts reaches
// the other side as an error, not a clean end of stream, because NetConn
// reports any close status other than a normal one as an error.
//
// The tunnel itself has no authentication. Anyone who can reach the server
// can reach its target, so serve it behind TLS and an authenticating proxy, or
// add a check of the request to the handler.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/picatz/websocket"
)

// dialTimeout bounds connecting to the target and the WebSocket handshake.
const dialTimeout = 10 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tunnel server|client [flags]")
	}
	fs := flag.NewFlagSet("tunnel "+args[0], flag.ExitOnError)
	switch args[0] {
	case "server":
		addr := fs.String("addr", "localhost:8080", "listen address for WebSocket connections")
		target := fs.String("target", "", "TCP `address` to forward each connection to")
		fs.Parse(args[1:])
		if *target == "" {
			return errors.New("server: -target is required")
		}
		srv := &http.Server{Addr: *addr, Handler: http.HandlerFunc(server(*target))}
		context.AfterFunc(ctx, func() { srv.Close() })
		log.Printf("forwarding ws://%s/ to %s", *addr, *target)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case "client":
		listen := fs.String("listen", "localhost:2222", "local TCP `address` to accept connections on")
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return errors.New("client: want the server's ws or wss URL")
		}
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		log.Printf("forwarding %s to %s", ln.Addr(), fs.Arg(0))
		return client(ctx, ln, fs.Arg(0))
	default:
		return fmt.Errorf("unknown command %q: want server or client", args[0])
	}
}

// server returns a handler that connects each WebSocket to target.
func server(target string) http.HandlerFunc {
	upgrader := &websocket.Upgrader{MaxMessageSize: 1 << 20}
	return func(w http.ResponseWriter, r *http.Request) {
		// Connect to the target first, so a client learns of a failure
		// from the HTTP status instead of an immediately closed tunnel.
		var d net.Dialer
		ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
		defer cancel()
		tcp, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			log.Printf("%s: %v", r.RemoteAddr, err)
			http.Error(w, "target unavailable", http.StatusBadGateway)
			return
		}
		ws, err := upgrader.Upgrade(w, r)
		if err != nil {
			tcp.Close()
			return // Upgrade has already written the HTTP error response.
		}
		join(websocket.NetConn(context.Background(), ws, websocket.BinaryMessage), tcp)
	}
}

// client accepts connections on ln and forwards each one to the tunnel
// server at url until ctx ends.
func client(ctx context.Context, ln net.Listener, url string) error {
	context.AfterFunc(ctx, func() { ln.Close() })
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		tcp, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		conns.Go(func() {
			dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
			ws, _, err := websocket.Dial(dialCtx, url)
			cancel()
			if err != nil {
				log.Printf("%s: %v", tcp.RemoteAddr(), err)
				tcp.Close()
				return
			}
			// ctx ending aborts the WebSocket, which ends join.
			join(websocket.NetConn(ctx, ws, websocket.BinaryMessage), tcp)
		})
	}
}

// join copies between a and b in both directions. When either direction
// ends it closes both connections, which ends the other, and returns once
// both copies have stopped.
func join(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	var copies sync.WaitGroup
	copies.Go(func() {
		defer closeBoth()
		io.Copy(a, b)
	})
	copies.Go(func() {
		defer closeBoth()
		io.Copy(b, a)
	})
	copies.Wait()
}
