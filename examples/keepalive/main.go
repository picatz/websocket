// Command keepalive is an echo server that pings each client on a fixed
// interval and drops clients that stop answering.
//
// RFC 6455 Section 5.5.2 lets either endpoint send a Ping, and the peer must
// answer with a Pong. Conn answers Pings automatically while ReadMessage runs,
// so a client only needs an active read loop to stay connected.
//
// The server calls Conn.Ping on a ticker. Ping waits for the matching Pong,
// which the handler's own read loop delivers. A client that stops reading, or
// whose network silently drops, lets the Ping time out, and the server closes
// the connection.
//
// [RFC 6455 Section 5.5.2]: https://www.rfc-editor.org/rfc/rfc6455.html#section-5.5.2
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/picatz/websocket"
)

// keepalive configures how often a server pings and how long it waits.
type keepalive struct {
	// pingInterval is the time between Pings.
	pingInterval time.Duration
	// pongWait is how long each Ping waits for its Pong before the server
	// gives up on the connection.
	pongWait time.Duration
	// writeWait bounds each echoed message.
	writeWait time.Duration
}

// handler returns an echo handler that applies k to every connection.
func (k keepalive) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			upgradeError(w, err)
			return
		}
		defer conn.Close()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		var wg sync.WaitGroup
		defer wg.Wait()
		wg.Go(func() {
			ticker := time.NewTicker(k.pingInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				pingCtx, cancelPing := context.WithTimeout(ctx, k.pongWait)
				err := conn.Ping(pingCtx)
				cancelPing()
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						log.Printf("client %s missed its pong; dropping", r.RemoteAddr)
					}
					// Closing interrupts the blocked ReadMessage below.
					conn.Close()
					return
				}
			}
		})

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			writeCtx, cancelWrite := context.WithTimeout(ctx, k.writeWait)
			err = conn.WriteMessageContext(writeCtx, messageType, message)
			cancelWrite()
			if err != nil {
				return
			}
		}
	}
}

func upgradeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, websocket.ErrOriginNotAllowed):
		http.Error(w, "WebSocket origin not allowed", http.StatusForbidden)
	case errors.Is(err, websocket.ErrUnsupportedVersion):
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "Unsupported WebSocket version", http.StatusUpgradeRequired)
	case !errors.Is(err, websocket.ErrHandshakeFailed):
		// A failed hijack or 101 write may already own the transport.
		http.Error(w, "Invalid WebSocket handshake", http.StatusBadRequest)
	}
}

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	k := keepalive{pingInterval: 20 * time.Second, pongWait: 10 * time.Second, writeWait: 5 * time.Second}
	http.Handle("/ws", k.handler())
	log.Printf("keepalive server listening on ws://%s/ws", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
