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
	"time"

	"github.com/picatz/websocket"
)

// TestClosedErrorsMatchNetErrClosed checks that every way of using a closed
// Conn reports the standard library's net.ErrClosed, so callers need one
// check, while io.ErrClosedPipe and ErrAlreadyClosed keep matching for code
// written against earlier versions.
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
		"control frame":     c.WriteControlFrame(websocket.PingMessage, nil),
		"ping":              c.Ping(ctx),
		"second Close":      c.Close(),
		"CloseWithStatus":   c.CloseWithStatus(websocket.StatusGoingAway, ""),
		"read after Closes": readErr(),
	} {
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s: err = %v, want a match for net.ErrClosed", name, err)
		}
		if name != "interrupted read" && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("%s: err = %v, want a match for io.ErrClosedPipe", name, err)
		}
	}
	if err := c.Close(); !errors.Is(err, websocket.ErrAlreadyClosed) {
		t.Errorf("second Close: err = %v, want ErrAlreadyClosed", err)
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
