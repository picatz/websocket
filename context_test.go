package websocket_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
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
	if _, _, err := c.ReadMessage(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after cancellation = %v, want net.ErrClosed", err)
	}
	if err := c.WriteMessage(context.Background(), websocket.TextMessage, []byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after cancellation = %v, want net.ErrClosed", err)
	}
	if err := c.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Close after cancellation = %v, want net.ErrClosed", err)
	}
}

func TestReadMessageCtxCancelAbortsBlockedRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := contextPipe(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, _, err := client.ReadMessage(ctx)
			done <- err
		}()
		synctest.Wait() // the read is blocked in the transport
		cancel()
		if err := contextAwait(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		requireContextTerminal(t, client)
		// The peer observes an abnormal end of stream, not a closing handshake.
		if _, _, err := server.ReadMessage(context.Background()); err == nil {
			t.Fatal("peer read succeeded after abort")
		}
	})
}

func TestReadMessageCtxDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, _ := contextPipe(t)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, err := client.ReadMessage(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		// The bubble's clock is exact: the read ends at the deadline, not
		// after a further transport timeout.
		if elapsed := time.Since(start); elapsed != 20*time.Millisecond {
			t.Fatalf("read returned after %v, want 20ms", elapsed)
		}
		requireContextTerminal(t, client)
	})
}

func TestReadMessageCtxPreservesCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, _ := contextPipe(t)
		cause := errors.New("synthetic shutdown")
		ctx, cancel := context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() {
			_, _, err := client.ReadMessage(ctx)
			done <- err
		}()
		synctest.Wait()
		cancel(cause)
		if err := contextAwait(t, done); !errors.Is(err, cause) {
			t.Fatalf("err = %v, want cause", err)
		}
	})
}

func TestContextAlreadyDoneLeavesConnectionUsable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := contextPipe(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, _, err := client.ReadMessage(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("read err = %v, want context.Canceled", err)
		}
		if err := client.WriteMessage(ctx, websocket.TextMessage, []byte("x")); !errors.Is(err, context.Canceled) {
			t.Fatalf("write err = %v, want context.Canceled", err)
		}

		// Neither call touched the connection: a full exchange still works.
		sent := make(chan error, 1)
		go func() { sent <- server.WriteMessage(context.Background(), websocket.TextMessage, []byte("hello")) }()
		kind, data, err := client.ReadMessage(t.Context())
		if err != nil || kind != websocket.TextMessage || string(data) != "hello" {
			t.Fatalf("read = %v %q %v", kind, data, err)
		}
		if err := contextAwait(t, sent); err != nil {
			t.Fatal(err)
		}
	})
}

func TestContextMessagesRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := contextPipe(t)
		ctx := t.Context()
		for i := range 3 {
			got := make(chan []byte, 1)
			go func() {
				_, data, err := server.ReadMessage(ctx)
				if err != nil {
					t.Error(err)
				}
				got <- data
			}()
			payload := []byte{byte('a' + i)}
			if err := client.WriteMessage(ctx, websocket.BinaryMessage, payload); err != nil {
				t.Fatal(err)
			}
			if data := contextAwait(t, got); string(data) != string(payload) {
				t.Fatalf("data = %q, want %q", data, payload)
			}
		}
		// Completed operations leave no cancellation armed: a normal Close works.
		go server.ReadMessage(context.Background()) // consume the close frame
		if err := client.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
	})
}

func TestWriteMessageCtxCancelAbortsBlockedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, _ := contextPipe(t) // nobody reads from the peer, so the write blocks
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- client.WriteMessage(ctx, websocket.TextMessage, []byte("stalled")) }()
		synctest.Wait()
		cancel()
		if err := contextAwait(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		requireContextTerminal(t, client)
	})
}

func TestWriteMessageCtxCancelWhileWaitingForWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, _ := contextPipe(t)
		first := make(chan error, 1)
		go func() {
			first <- client.WriteMessage(context.Background(), websocket.TextMessage, []byte("holds the writer"))
		}()
		synctest.Wait()

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if err := client.WriteMessage(ctx, websocket.TextMessage, []byte("queued")); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued write err = %v, want context.DeadlineExceeded", err)
		}
		// Aborting the connection also releases the stalled writer.
		if err := contextAwait(t, first); err == nil {
			t.Fatal("stalled writer succeeded after abort")
		}
	})
}

// mustNewConn is websocket.NewConn for configurations that cannot fail.
func mustNewConn(conn net.Conn, cfg websocket.ConnConfig) *websocket.Conn {
	c, err := websocket.NewConn(conn, cfg)
	if err != nil {
		panic(err)
	}
	return c
}

// NextReader's context bounds the whole message: ending it partway through
// aborts the connection, and the reader reports the context's cause.
func TestNextReaderCtxCancelMidMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, peer := newScriptedPeer(t)
		// A binary frame without FIN: the message never finishes.
		peer.send(append([]byte{0x02, 5}, "first"...))
		ctx, cancel := context.WithCancel(t.Context())
		_, r, err := client.NextReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := r.Read(buf)
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := contextAwait(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("Read err = %v, want context.Canceled", err)
		}
		requireContextTerminal(t, client)
	})
}

// NextWriter's context bounds the whole message, including each Write.
func TestNextWriterCtxCancelMidMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, _ := contextPipe(t) // Nobody reads the peer, so writes block.
		ctx, cancel := context.WithCancel(t.Context())
		w, err := client.NextWriter(ctx, websocket.BinaryMessage)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := w.Write(make([]byte, 128<<10))
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := contextAwait(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("Write err = %v, want context.Canceled", err)
		}
		requireContextTerminal(t, client)
	})
}

// Once a NextReader or NextWriter message ends, its context no longer
// matters: ending it later leaves the connection open.
func TestStreamCtxReleasedWhenMessageEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := contextPipe(t)
		echoed := make(chan error, 1)
		go func() {
			for range 2 {
				typ, data, err := server.ReadMessage(context.Background())
				if err == nil {
					err = server.WriteMessage(context.Background(), typ, data)
				}
				if err != nil {
					echoed <- err
					return
				}
			}
			echoed <- nil
		}()

		wctx, cancelWrite := context.WithCancel(t.Context())
		w, err := client.NextWriter(wctx, websocket.TextMessage)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		cancelWrite()

		rctx, cancelRead := context.WithCancel(t.Context())
		_, r, err := client.NextReader(rctx)
		if err != nil {
			t.Fatal(err)
		}
		if data, err := io.ReadAll(r); err != nil || string(data) != "hello" {
			t.Fatalf("ReadAll = %q, %v", data, err)
		}
		cancelRead()
		synctest.Wait()

		if err := client.WriteMessage(context.Background(), websocket.TextMessage, []byte("still open")); err != nil {
			t.Fatalf("write after both contexts ended: %v", err)
		}
		if _, data, err := client.ReadMessage(context.Background()); err != nil || string(data) != "still open" {
			t.Fatalf("read after both contexts ended = %q, %v", data, err)
		}
		if err := contextAwait(t, echoed); err != nil {
			t.Fatal(err)
		}
	})
}
