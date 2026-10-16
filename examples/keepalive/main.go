// Command keepalive is an echo server that pings each client on a fixed
// interval and drops clients that stop answering.
//
// RFC 6455 Section 5.5.2 lets either endpoint send a Ping, and the peer must
// answer with a Pong. Conn answers Pings automatically while ReadMessage runs,
// so a client only needs an active read loop to stay connected.
//
// The server keeps a read deadline one pongWait ahead and extends it from the
// Pong handler. A client that stops reading, or whose network silently drops,
// lets the deadline pass and ReadMessage fails, ending the handler.
//
// [RFC 6455 Section 5.5.2]: https://www.rfc-editor.org/rfc/rfc6455.html#section-5.5.2
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/picatz/websocket"
)

// keepalive configures how often a server pings and how long it waits.
type keepalive struct {
	// pingInterval is the time between Pings. It must be less than pongWait
	// so a healthy client always answers before the deadline.
	pingInterval time.Duration
	// pongWait is how long the server waits for any frame from the client
	// before it gives up on the connection.
	pongWait time.Duration
	// writeWait bounds each write, including Pings.
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

		// Pings and echoes are written from different goroutines. Writes are
		// already serialized by Conn, but both also set the shared write
		// deadline, so take a lock around the pair.
		var writeMu sync.Mutex
		write := func(write func() error) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			if err := conn.SetWriteDeadline(time.Now().Add(k.writeWait)); err != nil {
				return err
			}
			return write()
		}

		if err := conn.SetReadDeadline(time.Now().Add(k.pongWait)); err != nil {
			return
		}
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(k.pongWait))
		})

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
					err := write(func() error { return conn.WriteControlFrame(websocket.PingMessage, nil) })
					if err != nil {
						// Closing interrupts the blocked ReadMessage below.
						conn.Close()
						return
					}
				}
			}
		})

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					log.Printf("client %s missed its pong; dropping", r.RemoteAddr)
				}
				return
			}
			if err := write(func() error { return conn.WriteMessage(messageType, message) }); err != nil {
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

	k := keepalive{pingInterval: 20 * time.Second, pongWait: 30 * time.Second, writeWait: 5 * time.Second}
	http.Handle("/ws", k.handler())
	log.Printf("keepalive server listening on ws://%s/ws", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
