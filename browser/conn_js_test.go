//go:build js && wasm

package browser_test

import (
	"bytes"
	"context"
	"errors"
	"io"
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

func dial(t *testing.T, path string, opts ...browser.DialOption) *browser.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := browser.Dial(ctx, serverURL(t, path), opts...)
	if err != nil {
		t.Fatalf("Dial %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestEcho(t *testing.T) {
	conn := dial(t, "/echo")
	binary := []byte{0, 1, 2, 0xff}
	for _, tc := range []struct {
		opcode websocket.Opcode
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
	conn := dial(t, "/echo")
	if err := conn.WriteMessage(websocket.TextMessage, []byte{0xff}); !errors.Is(err, websocket.ErrInvalidFrame) {
		t.Fatalf("invalid UTF-8 text: err = %v, want ErrInvalidFrame", err)
	}
	if err := conn.WriteMessage(websocket.PingMessage, nil); !errors.Is(err, websocket.ErrInvalidOpcode) {
		t.Fatalf("ping: err = %v, want ErrInvalidOpcode", err)
	}
}

func TestSubprotocol(t *testing.T) {
	conn := dial(t, "/subprotocol", browser.WithSubprotocols("superchat", "chat"))
	if got := conn.Subprotocol(); got != "chat" {
		t.Fatalf("Subprotocol = %q, want chat", got)
	}
	if got := dial(t, "/echo").Subprotocol(); got != "" {
		t.Fatalf("Subprotocol without offer = %q, want empty", got)
	}
}

func TestPeerClose(t *testing.T) {
	conn := dial(t, "/goodbye")
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
	if err := conn.WriteMessage(websocket.TextMessage, []byte("late")); err != io.ErrClosedPipe {
		t.Fatalf("write after peer close: err = %v, want io.ErrClosedPipe", err)
	}
}

func TestClose(t *testing.T) {
	conn := dial(t, "/echo")
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.Close(); !errors.Is(err, websocket.ErrAlreadyClosed) {
		t.Fatalf("second Close: err = %v, want ErrAlreadyClosed", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("x")); err != io.ErrClosedPipe {
		t.Fatalf("write after Close: err = %v, want io.ErrClosedPipe", err)
	}
	if _, _, err := conn.ReadMessage(); err != io.ErrClosedPipe {
		t.Fatalf("read after Close: err = %v, want io.ErrClosedPipe", err)
	}
}

func TestMaxMessageSize(t *testing.T) {
	conn := dial(t, "/echo", browser.WithMaxMessageSize(4))
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); !errors.Is(err, websocket.ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
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
	conn := dial(t, "/silent")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := conn.ReadMessageContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	// The canceled read closed the connection.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("x")); err != io.ErrClosedPipe {
		t.Fatalf("write after canceled read: err = %v, want io.ErrClosedPipe", err)
	}
}

func TestCloseWithStatus(t *testing.T) {
	conn := dial(t, "/echo")
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
	conn := dial(t, "/closestatus")
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
// with either. Subprotocol joins it once websocket.Conn has that method.
type conn interface {
	ReadMessage() (websocket.Opcode, []byte, error)
	ReadMessageContext(context.Context) (websocket.Opcode, []byte, error)
	WriteMessage(websocket.Opcode, []byte) error
	Close() error
	CloseWithStatus(websocket.StatusCode, string) error
}

var (
	_ conn = (*browser.Conn)(nil)
	_ conn = (*websocket.Conn)(nil)
)
