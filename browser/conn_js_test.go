//go:build js && wasm

package browser_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/picatz/websocket"
	"github.com/picatz/websocket/browser"
)

// serverURL returns the test server URL for path. TestNode sets it; run
// directly, without a server, the tests skip.
func serverURL(t *testing.T, path string) string {
	t.Helper()
	base := os.Getenv("BROWSER_TEST_URL")
	if base == "" {
		t.Skip("BROWSER_TEST_URL not set; run go test ./browser on the host")
	}
	return base + path
}

func dial(t *testing.T, path string, d *browser.Dialer) *browser.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := d.Dial(ctx, serverURL(t, path))
	if err != nil {
		t.Fatalf("Dial %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestEcho(t *testing.T) {
	conn := dial(t, "/echo", nil)
	binary := []byte{0, 1, 2, 0xff}
	for _, tc := range []struct {
		opcode websocket.MessageType
		data   []byte
	}{
		{websocket.TextMessage, []byte("hello, 世界")},
		{websocket.BinaryMessage, binary},
		{websocket.TextMessage, []byte{}},
		{websocket.BinaryMessage, bytes.Repeat([]byte{7}, 70000)},
	} {
		if err := conn.WriteMessage(tc.opcode, tc.data); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
		opcode, data, err := conn.ReadMessage()
		if err != nil || opcode != tc.opcode || !bytes.Equal(data, tc.data) {
			t.Fatalf("ReadMessage = %v, %d bytes, %v; want %v, %d bytes", opcode, len(data), err, tc.opcode, len(tc.data))
		}
	}
}

func TestWriteValidation(t *testing.T) {
	conn := dial(t, "/echo", nil)
	if err := conn.WriteMessage(websocket.TextMessage, []byte{0xff}); !errors.Is(err, websocket.ErrInvalidFrame) {
		t.Fatalf("invalid UTF-8 text: err = %v, want ErrInvalidFrame", err)
	}
	if err := conn.WriteMessage(websocket.MessageType(9), nil); err == nil {
		t.Fatal("WriteMessage accepted an invalid message type")
	}
}

func TestSubprotocol(t *testing.T) {
	conn := dial(t, "/subprotocol", &browser.Dialer{Subprotocols: []string{"superchat", "chat"}})
	if got := conn.Subprotocol(); got != "chat" {
		t.Fatalf("Subprotocol = %q, want chat", got)
	}
	if got := dial(t, "/echo", nil).Subprotocol(); got != "" {
		t.Fatalf("Subprotocol without offer = %q, want empty", got)
	}
}

func TestPeerClose(t *testing.T) {
	conn := dial(t, "/goodbye", nil)
	// Messages that arrived before the close are still delivered.
	for _, want := range []string{"one", "two"} {
		if _, data, err := conn.ReadMessage(); err != nil || string(data) != want {
			t.Fatalf("ReadMessage = %q, %v; want %q", data, err, want)
		}
	}
	_, _, err := conn.ReadMessage()
	if !errors.Is(err, io.EOF) || websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("after peer close: err = %v, want a CloseError with StatusGoingAway", err)
	}
	if ce, ok := errors.AsType[*websocket.CloseError](err); !ok || ce.Reason != "bye" {
		t.Fatalf("after peer close: err = %#v, want reason %q", err, "bye")
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after peer close: err = %v, want net.ErrClosed", err)
	}
}

func TestClose(t *testing.T) {
	conn := dial(t, "/echo", nil)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.Close(); !errors.Is(err, websocket.ErrAlreadyClosed) {
		t.Fatalf("second Close: err = %v, want ErrAlreadyClosed", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after Close: err = %v, want net.ErrClosed", err)
	}
	if _, _, err := conn.ReadMessage(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after Close: err = %v, want net.ErrClosed", err)
	}
}

func TestMaxMessageSize(t *testing.T) {
	conn := dial(t, "/echo", &browser.Dialer{MaxMessageSize: 4})
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); !errors.Is(err, websocket.ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}

	// A negative limit means none.
	conn = dial(t, "/echo", &browser.Dialer{MaxMessageSize: -1})
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "12345" {
		t.Fatalf("unlimited: ReadMessage = %q, %v", data, err)
	}
}

func TestDialFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := browser.Dial(ctx, serverURL(t, "/reject")); !errors.Is(err, browser.ErrDialFailed) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rejected handshake: err = %v, want ErrDialFailed", err)
	}
	if _, err := browser.Dial(ctx, "not a url"); !errors.Is(err, browser.ErrDialFailed) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("malformed URL: err = %v, want ErrDialFailed", err)
	}
}

func TestReadContext(t *testing.T) {
	conn := dial(t, "/silent", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := conn.ReadMessageContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	// The canceled read closed the connection.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after canceled read: err = %v, want net.ErrClosed", err)
	}
}

func TestCloseWithStatus(t *testing.T) {
	conn := dial(t, "/echo", nil)
	for _, tc := range []struct {
		code   websocket.StatusCode
		reason string
	}{
		{websocket.StatusGoingAway, ""}, // valid on the wire, but hosts refuse it
		{2999, ""},
		{5000, ""},
		{4000, strings.Repeat("x", 124)},
		{4000, "\xff"},
	} {
		if err := conn.CloseWithStatus(tc.code, tc.reason); !errors.Is(err, websocket.ErrInvalidCloseStatus) {
			t.Fatalf("CloseWithStatus(%d, %d bytes) = %v, want ErrInvalidCloseStatus", tc.code, len(tc.reason), err)
		}
	}
	// Rejected statuses leave the connection open.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("WriteMessage after rejected status: %v", err)
	}
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "ping" {
		t.Fatalf("ReadMessage = %q, %v", data, err)
	}

	if err := conn.CloseWithStatus(4000, strings.Repeat("x", 123)); err != nil {
		t.Fatalf("CloseWithStatus: %v", err)
	}
	if err := conn.CloseWithStatus(4000, ""); !errors.Is(err, websocket.ErrAlreadyClosed) {
		t.Fatalf("second close: err = %v, want ErrAlreadyClosed", err)
	}
}

func TestCloseWithStatusReachesPeer(t *testing.T) {
	conn := dial(t, "/closestatus", nil)
	if err := conn.CloseWithStatus(4321, "done"); err != nil {
		t.Fatal(err)
	}
	// Wait for the handshake to finish; the server records what it received.
	_, _, _ = conn.ReadMessage()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	check, err := browser.Dial(ctx, serverURL(t, "/lastclose"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if _, data, err := check.ReadMessage(); err != nil || string(data) != "4321 done" {
		t.Fatalf("server saw %q, %v; want %q", data, err, "4321 done")
	}
}

// conn is the method set shared by browser.Conn and websocket.Conn. Both must
// implement it with identical signatures, so code written against it works
// with either.
type conn interface {
	ReadMessage() (websocket.MessageType, []byte, error)
	ReadMessageContext(context.Context) (websocket.MessageType, []byte, error)
	WriteMessage(websocket.MessageType, []byte) error
	WriteMessageContext(context.Context, websocket.MessageType, []byte) error
	Close() error
	CloseWithStatus(websocket.StatusCode, string) error
	Subprotocol() string
}

var (
	_ conn = (*browser.Conn)(nil)
	_ conn = (*websocket.Conn)(nil)
)

func TestWriteBackpressure(t *testing.T) {
	const mark = 64 << 10
	defer browser.SetWriteHighWaterMark(mark)()
	conn := dial(t, "/stall", nil)

	// The server never reads, so once the TCP buffers fill the host's send
	// queue grows past the mark and the next write must wait.
	chunk := make([]byte, 256<<10)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var err error
	for range 1000 {
		if err = conn.WriteMessageContext(ctx, websocket.BinaryMessage, chunk); err != nil {
			break
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded once the send queue is full", err)
	}
	if n := browser.BufferedAmount(conn); n <= mark {
		t.Fatalf("BufferedAmount = %d, want more than the %d byte mark", n, mark)
	}
	// Nothing was sent by the timed-out write, so the connection stays open.
	if err := conn.Close(); err != nil {
		t.Fatalf("Close after timed-out write: %v", err)
	}
}
