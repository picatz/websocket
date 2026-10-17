package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

var testKeepalive = keepalive{
	pingInterval: 10 * time.Millisecond,
	pongWait:     100 * time.Millisecond,
	writeWait:    time.Second,
}

// TestReadingClientStaysConnected checks that a client with an active read
// loop answers Pings and outlives several pongWait periods.
func TestReadingClientStaysConnected(t *testing.T) {
	srv := httptest.NewServer(testKeepalive.handler())
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(), "ws"+srv.URL[len("http"):])
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Count Pings, then answer them as the default handler would.
	var pings atomic.Int64
	conn.SetPingHandler(func(appData string) error {
		pings.Add(1)
		return conn.WriteControlFrame(websocket.PongMessage, []byte(appData))
	})

	// The read loop is what answers Pings.
	messages := make(chan string)
	go func() {
		defer close(messages)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			messages <- string(msg)
		}
	}()

	time.Sleep(5 * testKeepalive.pongWait)
	if err := conn.WriteMessage(websocket.TextMessage, []byte("still here")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	select {
	case msg, ok := <-messages:
		if !ok || msg != "still here" {
			t.Fatalf("echo = %q (open %t), want %q", msg, ok, "still here")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no echo")
	}
	if pings.Load() < 2 {
		t.Fatalf("server sent %d Pings, want several", pings.Load())
	}
}

// TestSilentClientIsDropped checks that the server ends the connection when a
// client never reads, so never answers a Ping.
func TestSilentClientIsDropped(t *testing.T) {
	h := testKeepalive.handler()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h(w, r)
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(), "ws"+srv.URL[len("http"):])
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server kept a client that never answered Pings")
	}
}
