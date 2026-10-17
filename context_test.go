package websocket_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

func contextPipe(t *testing.T) (client, server *websocket.Conn) {
	t.Helper()
	a, b := net.Pipe()
	client = mustNewConn(a, websocket.ConnConfig{})
	server = mustNewConn(b, websocket.ConnConfig{Server: true})
	// Close the raw pipes first: on a synchronous pipe, two Close calls would
	// otherwise each wait out the one-second close-notification bound.
	t.Cleanup(func() {
		a.Close()
		b.Close()
		client.Close()
		server.Close()
	})
	return client, server
}

func contextAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not return")
		panic("unreachable")
	}
}

// requireContextTerminal checks the connection behaves as after Close.
func requireContextTerminal(t *testing.T, c *websocket.Conn) {
	t.Helper()
	if _, _, err := c.ReadMessage(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after cancellation = %v, want net.ErrClosed", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after cancellation = %v, want net.ErrClosed", err)
	}
	if err := c.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Close after cancellation = %v, want net.ErrClosed", err)
	}
}

func TestReadMessageContextCancelAbortsBlockedRead(t *testing.T) {
	client, server := contextPipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := client.ReadMessageContext(ctx)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond) // let the read block; correctness does not depend on it
	cancel()
	if err := contextAwait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	requireContextTerminal(t, client)
	// The peer observes an abnormal end of stream, not a closing handshake.
	if _, _, err := server.ReadMessage(); err == nil {
		t.Fatal("peer read succeeded after abort")
	}
}

func TestReadMessageContextDeadline(t *testing.T) {
	client, _ := contextPipe(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := client.ReadMessageContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("read returned after %v", elapsed)
	}
	requireContextTerminal(t, client)
}

func TestReadMessageContextPreservesCause(t *testing.T) {
	client, _ := contextPipe(t)
	cause := errors.New("synthetic shutdown")
	ctx, cancel := context.WithCancelCause(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := client.ReadMessageContext(ctx)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel(cause)
	if err := contextAwait(t, done); !errors.Is(err, cause) {
		t.Fatalf("err = %v, want cause", err)
	}
}

func TestContextAlreadyDoneLeavesConnectionUsable(t *testing.T) {
	client, server := contextPipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := client.ReadMessageContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("read err = %v, want context.Canceled", err)
	}
	if err := client.WriteMessageContext(ctx, websocket.TextMessage, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write err = %v, want context.Canceled", err)
	}

	// Neither call touched the connection: a full exchange still works.
	sent := make(chan error, 1)
	go func() { sent <- server.WriteMessage(websocket.TextMessage, []byte("hello")) }()
	kind, data, err := client.ReadMessageContext(t.Context())
	if err != nil || kind != websocket.TextMessage || string(data) != "hello" {
		t.Fatalf("read = %v %q %v", kind, data, err)
	}
	if err := contextAwait(t, sent); err != nil {
		t.Fatal(err)
	}
}

func TestContextMessagesRoundTrip(t *testing.T) {
	client, server := contextPipe(t)
	ctx := t.Context()
	for i := range 3 {
		got := make(chan []byte, 1)
		go func() {
			_, data, err := server.ReadMessageContext(ctx)
			if err != nil {
				t.Error(err)
			}
			got <- data
		}()
		payload := []byte{byte('a' + i)}
		if err := client.WriteMessageContext(ctx, websocket.BinaryMessage, payload); err != nil {
			t.Fatal(err)
		}
		if data := contextAwait(t, got); string(data) != string(payload) {
			t.Fatalf("data = %q, want %q", data, payload)
		}
	}
	// Completed operations leave no cancellation armed: a normal Close works.
	go server.ReadMessage() // consume the close frame
	if err := client.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestWriteMessageContextCancelAbortsBlockedWrite(t *testing.T) {
	client, _ := contextPipe(t) // nobody reads from the peer, so the write blocks
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.WriteMessageContext(ctx, websocket.TextMessage, []byte("stalled")) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := contextAwait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	requireContextTerminal(t, client)
}

func TestWriteMessageContextCancelWhileWaitingForWriter(t *testing.T) {
	client, _ := contextPipe(t)
	first := make(chan error, 1)
	go func() { first <- client.WriteMessage(websocket.TextMessage, []byte("holds the writer")) }()
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := client.WriteMessageContext(ctx, websocket.TextMessage, []byte("queued")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued write err = %v, want context.DeadlineExceeded", err)
	}
	// Aborting the connection also releases the stalled writer.
	if err := contextAwait(t, first); err == nil {
		t.Fatal("stalled writer succeeded after abort")
	}
}

// mustNewConn is websocket.NewConn for configurations that cannot fail.
func mustNewConn(conn net.Conn, cfg websocket.ConnConfig) *websocket.Conn {
	c, err := websocket.NewConn(conn, cfg)
	if err != nil {
		panic(err)
	}
	return c
}
