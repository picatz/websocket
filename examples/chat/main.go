// Command chat is a broadcast chat server: every text message a client sends
// is delivered to every connected client, including the sender.
//
// Each client gets a buffered send queue drained by its own writer goroutine,
// so one slow reader cannot stall the broadcast for everyone else. A client
// whose queue fills is disconnected instead of being allowed to grow memory
// without bound.
//
// Connect with a name, for example ws://localhost:8080/ws?name=ada.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/picatz/websocket"
)

const (
	// sendQueue is how many outgoing messages a client may have pending
	// before it is considered too slow and disconnected.
	sendQueue = 16
	// writeWait bounds each write to a client.
	writeWait = 5 * time.Second
	// maxMessage bounds each incoming chat message.
	maxMessage = 4 << 10
)

// client is one connected participant.
type client struct {
	name string
	send chan []byte
}

// hub tracks connected clients and fans messages out to them.
type hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[*client]struct{})}
}

// join adds c to the hub and announces it.
func (h *hub) join(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.broadcast(fmt.Appendf(nil, "* %s joined", c.name))
}

// leave removes c from the hub, closing its send queue, and announces it.
// It is safe to call after the hub already dropped c.
func (h *hub) leave(c *client) {
	h.mu.Lock()
	_, ok := h.clients[c]
	if ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
	if ok {
		h.broadcast(fmt.Appendf(nil, "* %s left", c.name))
	}
}

// broadcast queues msg for every client. A client whose queue is full is
// dropped: closing its queue makes its writer close the connection.
func (h *hub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			delete(h.clients, c)
			close(c.send)
		}
	}
}

// handler upgrades a request and runs one client until it disconnects.
func (h *hub) handler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.RemoteAddr
	}
	conn, err := (&websocket.Upgrader{MaxMessageSize: maxMessage}).Upgrade(w, r)
	if err != nil {
		// Upgrade has already written the HTTP error response.
		return
	}
	defer conn.Close()

	c := &client{name: name, send: make(chan []byte, sendQueue)}

	// The writer is the only goroutine that writes to conn. It exits when
	// the queue is closed, by leave or by a dropped slow client, or when a
	// write fails. Either way it closes conn, which ends the read loop.
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		defer conn.Close()
		for msg := range c.send {
			ctx, cancel := context.WithTimeout(r.Context(), writeWait)
			err := conn.WriteMessage(ctx, websocket.TextMessage, msg)
			cancel()
			if err != nil {
				return
			}
		}
	})

	h.join(c)
	defer h.leave(c)
	for {
		messageType, msg, err := conn.ReadMessage(r.Context())
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		h.broadcast(fmt.Appendf(nil, "%s: %s", c.name, msg))
	}
}

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	http.HandleFunc("/ws", newHub().handler)
	log.Printf("chat server listening on ws://%s/ws?name=you", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
