package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

func dial(t *testing.T, srv *httptest.Server, name string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), "ws"+srv.URL[len("http"):]+"?name="+name)
	if err != nil {
		t.Fatalf("Dial %s: %v", name, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func expect(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage(context.Background())
	if err != nil || string(msg) != want {
		t.Fatalf("ReadMessage = %q, %v; want %q", msg, err, want)
	}
}

func TestBroadcast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(newHub().handler))
	defer srv.Close()

	// Waiting for each join announcement orders the test: a client that has
	// seen its own announcement is registered with the hub.
	ada := dial(t, srv, "ada")
	expect(t, ada, "* ada joined")
	bob := dial(t, srv, "bob")
	expect(t, ada, "* bob joined")
	expect(t, bob, "* bob joined")

	if err := ada.WriteMessage(context.Background(), websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	expect(t, ada, "ada: hello")
	expect(t, bob, "ada: hello")

	bob.Close()
	expect(t, ada, "* bob left")
}

func TestSlowClientIsDropped(t *testing.T) {
	h := newHub()
	slow := &client{name: "slow", send: make(chan []byte, 1)}
	h.mu.Lock()
	h.clients[slow] = struct{}{}
	h.mu.Unlock()

	h.broadcast([]byte("one")) // fills the queue
	h.broadcast([]byte("two")) // overflows it

	h.mu.Lock()
	_, ok := h.clients[slow]
	h.mu.Unlock()
	if ok {
		t.Fatal("slow client is still registered")
	}
	if msg := <-slow.send; string(msg) != "one" {
		t.Fatalf("queued = %q, want %q", msg, "one")
	}
	if _, open := <-slow.send; open {
		t.Fatal("slow client's queue is still open")
	}
	h.leave(slow) // must not close the queue twice
}
