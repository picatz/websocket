package websocket_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// A server that fails a connection usually leaves the rest of the offending
// message unread. Closing a TCP connection with unread data resets it, and a
// peer that receives the reset may discard the Close frame before reading it,
// as Windows does. The failing side must instead half-close and drain, so
// the peer reads the Close frame and then a clean EOF.
func TestFailureCloseIsNotReset(t *testing.T) {
	const limit = 1 << 10
	serverErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{MaxMessageSize: limit}).Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, err = c.ReadMessage(context.Background())
		serverErr <- err
	}))
	t.Cleanup(srv.Close)

	var raw net.Conn
	d := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		raw = c
		return c, err
	}}
	c, _, err := d.Dial(t.Context(), wsURL(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// A masked binary frame declaring far more than the limit, with part
	// of its payload: the server refuses it from the header and never
	// reads the payload.
	frame := []byte{0x82, 0x80 | 127}
	frame = binary.BigEndian.AppendUint64(frame, 1<<20)
	frame = append(frame, 1, 2, 3, 4)
	frame = append(frame, make([]byte, 64<<10)...)
	if _, err := raw.Write(frame); err != nil {
		t.Fatal(err)
	}

	raw.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(bufio.NewReader(raw))
	if err != nil {
		t.Fatalf("read after the server's Close: %v, want a clean EOF (got %x first)", err, got)
	}
	if want := []byte{0x88, 0x02, 0x03, 0xf1}; !bytes.Equal(got, want) {
		t.Fatalf("server sent %x, want the 1009 Close frame %x", got, want)
	}
	raw.Close()
	if err := <-serverErr; !errors.Is(err, websocket.ErrMessageTooLarge) {
		t.Fatalf("server read error = %v, want ErrMessageTooLarge", err)
	}
}
