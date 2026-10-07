package websocket

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestReadMessageTruncatedFrameIsUnexpectedEOF(t *testing.T) {
	tests := []struct {
		name   string
		wire   []byte
		server bool
	}{
		{"header", []byte{0x82}, false},
		{"short length missing", []byte{0x82, 126}, false},
		{"short length partial", []byte{0x82, 126, 0}, false},
		{"long length missing", []byte{0x82, 127}, false},
		{"long length partial", []byte{0x82, 127, 0, 0, 0}, false},
		{"mask missing", []byte{0x82, 0x80}, true},
		{"mask partial", []byte{0x82, 0x80, 1, 2}, true},
		{"payload missing", []byte{0x82, 1}, false},
		{"payload partial", []byte{0x82, 2, 'a'}, false},
		{"continuation missing", []byte{0x02, 1, 'a'}, false},
		{"continuation after pong missing", []byte{0x02, 1, 'a', 0x8a, 0}, false},
		{"empty continuation missing", []byte{0x02, 0, 0x00, 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := frameConn(tt.wire, tt.server, 0).ReadMessage()
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("ReadMessage() error = %v, want unexpected EOF", err)
			}
		})
	}
	_, _, err := frameConn(nil, false, 0).ReadMessage()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream error = %v, want EOF", err)
	}
}

func TestWriteMessagePreservesTimeoutCause(t *testing.T) {
	for _, size := range []int{0, 8192} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			raw, peer := net.Pipe()
			defer raw.Close()
			defer peer.Close()
			if err := raw.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			err := NewConn(raw, true, nil).WriteMessage(BinaryMessage, bytes.Repeat([]byte{'x'}, size))
			if !errors.Is(err, ErrWriteFailed) {
				t.Fatalf("error = %v, want ErrWriteFailed", err)
			}
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("error = %v, want deadline cause", err)
			}
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Errorf("error = %v, want net.Error timeout", err)
			}
		})
	}
}

func TestWriteErrorsPreserveClosedPipeCause(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(strconv.FormatBool(control), func(t *testing.T) {
			raw, peer := net.Pipe()
			defer raw.Close()
			peer.Close()
			conn := NewConn(raw, true, nil)
			var err error
			if control {
				err = conn.WriteControlFrame(PingMessage, []byte("ping"))
			} else {
				err = conn.WriteMessage(BinaryMessage, nil)
			}
			if !errors.Is(err, ErrWriteFailed) || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("error = %v, want ErrWriteFailed and closed pipe", err)
			}
			// bufio retains the transport failure; the next call fails while writing
			// its header rather than attempting to send another frame.
			err = conn.WriteMessage(BinaryMessage, nil)
			if !errors.Is(err, ErrWriteFailed) || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("subsequent error = %v, want ErrWriteFailed and closed pipe", err)
			}
		})
	}
}

func TestReadMessageEOFAtMessageBoundaries(t *testing.T) {
	for _, wire := range [][]byte{
		nil,
		{0x8a, 0}, // A pong does not begin a data message.
		{0x88, 0}, // A peer may close during a fragmented message.
		{0x02, 1, 'a', 0x88, 0},
	} {
		_, _, err := frameConn(wire, false, 0).ReadMessage()
		if err != io.EOF {
			t.Errorf("wire %x: error = %v, want EOF", wire, err)
		}
	}
	conn := frameConn([]byte{0x82, 1, 'a'}, false, 0)
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "a" {
		t.Fatalf("complete message = %q, %v", data, err)
	}
	if _, _, err := conn.ReadMessage(); err != io.EOF {
		t.Fatalf("error after complete message = %v, want EOF", err)
	}
}

// An extension failure is not a transport EOF, even if it wraps one.
type continuationErrorExtension struct {
	Extension
	err error
}

func (*continuationErrorExtension) IsEnabled() bool { return true }
func (*continuationErrorExtension) Name() string    { return "continuation-error" }
func (e *continuationErrorExtension) ProcessIncomingFrame(f *Frame) error {
	if f.Opcode == ContinuationFrame {
		return e.err
	}
	return nil
}
func TestReadMessagePreservesExtensionEOFCause(t *testing.T) {
	cause := fmt.Errorf("extension failed: %w", io.EOF)
	conn := frameConn([]byte{0x02, 1, 'a', 0x80, 1, 'b'}, false, 0)
	conn.extensions = []Extension{&continuationErrorExtension{err: cause}}
	_, _, err := conn.ReadMessage()
	if !errors.Is(err, cause) || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want extension cause", err)
	}
}
