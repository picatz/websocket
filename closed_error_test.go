package websocket_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/picatz/websocket"
)

// TestClosedErrorsMatchNetErrClosed checks that every way of using a closed
// Conn reports the standard library's net.ErrClosed, so callers need one
// check.
func TestClosedErrorsMatchNetErrClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
	}))
	defer srv.Close()

	c, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() {
		_, _, err := c.ReadMessage()
		blocked <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the read block in the transport
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	readErr := func() error { _, _, err := c.ReadMessage(); return err }
	for name, err := range map[string]error{
		"interrupted read":  <-blocked,
		"read":              readErr(),
		"write":             c.WriteMessage(websocket.TextMessage, []byte("x")),
		"write with ctx":    c.WriteMessageContext(ctx, websocket.TextMessage, []byte("x")),
		"ping":              c.Ping(ctx),
		"second Close":      c.Close(),
		"CloseWithStatus":   c.CloseWithStatus(websocket.StatusGoingAway, ""),
		"read after Closes": readErr(),
	} {
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s: err = %v, want a match for net.ErrClosed", name, err)
		}
	}
	if err := c.Close(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("second Close: err = %v, want net.ErrClosed", err)
	}
}

// TestPeerCloseThenClosedError checks that after the peer's Close frame has
// been reported as a *CloseError, later calls report net.ErrClosed rather than
// repeating the close.
func TestPeerCloseThenClosedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		_ = c.CloseWithStatus(websocket.StatusGoingAway, "bye")
	}))
	defer srv.Close()

	c, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.ReadMessage(); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("first read: err = %v, want StatusGoingAway", err)
	}
	if _, _, err := c.ReadMessage(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("read after peer close: err = %v, want net.ErrClosed", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("write after peer close: err = %v, want net.ErrClosed", err)
	}
}

// closeGate delays the transport's Close until release is closed, so the
// peer's reply to a Close frame reaches the reader first.
type closeGate struct {
	net.Conn
	release chan struct{}
}

func (g *closeGate) Close() error {
	<-g.release
	return g.Conn.Close()
}

// TestInterruptedReadAfterPeerReply checks that a read interrupted by Close
// reports net.ErrClosed even when the peer's Close reply is decoded before the
// transport closes, so a local Close looks the same on every run.
func TestInterruptedReadAfterPeerReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := net.Pipe()
		defer b.Close()
		gate := &closeGate{Conn: a, release: make(chan struct{})}
		c := mustNewConn(gate, websocket.ConnConfig{})

		blocked := make(chan error, 1)
		go func() {
			_, _, err := c.ReadMessage()
			blocked <- err
		}()

		// A Ping answered with a Pong proves the read is in progress: ReadMessage
		// keeps reading frames without rechecking for a local Close.
		if _, err := b.Write([]byte{0x89, 0x00}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(b, make([]byte, 6)); err != nil { // masked Pong
			t.Fatal(err)
		}
		// Wait for the reader to release the write lock it took for the Pong and
		// block for the next frame; Close skips its frame rather than wait
		// behind a writer.
		synctest.Wait()

		go func() {
			defer close(gate.release)
			// The client's masked Close: 2 header bytes, 4 mask bytes, 2 payload bytes.
			_ = b.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := io.ReadFull(b, make([]byte, 8)); err != nil {
				return
			}
			// net.Pipe returns once the reader has consumed the whole reply.
			_, _ = b.Write([]byte{0x88, 0x02, 0x03, 0xe8})
		}()

		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := <-blocked; !errors.Is(err, net.ErrClosed) {
			t.Fatalf("interrupted read: err = %v, want a match for net.ErrClosed", err)
		}
	})
}
