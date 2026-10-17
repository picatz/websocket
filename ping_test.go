package websocket_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// readLoop reads messages until the connection fails, forwarding them.
func readLoop(c *websocket.Conn) (<-chan []byte, <-chan error) {
	messages, done := make(chan []byte, 16), make(chan error, 1)
	go func() {
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			messages <- data
		}
	}()
	return messages, done
}

func TestPingRoundTrip(t *testing.T) {
	client, server := contextPipe(t)
	readLoop(server) // the server's reads answer Pings automatically
	readLoop(client) // the client's reads deliver the Pongs

	for range 3 {
		if err := client.Ping(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPingConcurrent(t *testing.T) {
	client, server := contextPipe(t)
	readLoop(server)
	readLoop(client)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { errs <- client.Ping(t.Context()) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPingTimeoutLeavesConnectionOpen(t *testing.T) {
	// The scripted peer reads every frame but never answers, so no Pong
	// arrives.
	client, p := newScriptedPeer(t)
	readLoop(client)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := client.Ping(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if f := p.next(t); f.opcode != wirePing {
		t.Fatalf("first frame = %#x, want a Ping", f.opcode)
	}
	// Waiting for a Pong is not terminal: data still flows.
	errc := make(chan error, 1)
	go func() { errc <- client.WriteMessage(websocket.TextMessage, []byte("still open")) }()
	if f := p.next(t); f.opcode != wireText || string(f.payload) != "still open" {
		t.Fatalf("peer got %#x %q, want the text message", f.opcode, f.payload)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestPingWithoutReaderWaitsForContext(t *testing.T) {
	client, server := contextPipe(t)
	readLoop(server)
	// No client read loop: the Pong is sent but never processed.
	cause := errors.New("synthetic keepalive budget")
	ctx, cancel := context.WithTimeoutCause(t.Context(), 30*time.Millisecond, cause)
	defer cancel()
	if err := client.Ping(ctx); !errors.Is(err, cause) {
		t.Fatalf("err = %v, want cause", err)
	}
}

func TestPingReturnsWhenConnectionCloses(t *testing.T) {
	// The scripted peer closes the connection instead of answering.
	client, p := newScriptedPeer(t)
	readLoop(client)
	done := make(chan error, 1)
	go func() { done <- client.Ping(t.Context()) }()
	if f := p.next(t); f.opcode != wirePing {
		t.Fatalf("first frame = %#x, want a Ping", f.opcode)
	}
	p.conn.Close()
	if err := contextAwait(t, done); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v, want net.ErrClosed", err)
	}
}

func TestPingAfterClose(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	closed := mustNewConn(a, websocket.ConnConfig{})
	go io.Copy(io.Discard, b)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.Ping(t.Context()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v, want net.ErrClosed", err)
	}
}

func TestPingContextAlreadyDone(t *testing.T) {
	client, server := contextPipe(t)
	serverMessages, _ := readLoop(server)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := client.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Nothing was written: the next frame the server sees is this message.
	if err := client.WriteMessage(websocket.TextMessage, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if got := contextAwait(t, serverMessages); string(got) != "first" {
		t.Fatalf("server got %q", got)
	}
}

func TestPingCancelDuringStalledWriteAborts(t *testing.T) {
	client, _ := contextPipe(t) // nobody reads from the peer, so the write blocks
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.Ping(ctx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := contextAwait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	requireContextTerminal(t, client)
}

func TestPingWithCompression(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{Compression: &websocket.Compression{}}).Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(kind, data); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	client, resp, err := (&websocket.Dialer{Compression: &websocket.Compression{}}).Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := resp.Header.Get("Sec-WebSocket-Extensions"); !strings.Contains(got, "permessage-deflate") {
		t.Fatalf("compression not negotiated: %q", got)
	}
	messages, _ := readLoop(client)
	payload := []byte(strings.Repeat("compressible ", 64))
	if err := client.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := contextAwait(t, messages); string(got) != string(payload) {
		t.Fatal("compressed echo mismatch")
	}
}
