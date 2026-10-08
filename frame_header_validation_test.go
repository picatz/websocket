package websocket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"
)

// headerOnlyConn allows buffered reads up to a known invalid header, then
// records any attempt to read further. Unlike a bytes.Reader containing a
// complete frame, it cannot prefetch the forbidden body during a header read.
type headerOnlyConn struct {
	memoryConn
	bodyReads int
}

func (c *headerOnlyConn) Read(p []byte) (int, error) {
	if c.Reader.Len() == 0 {
		c.bodyReads++
		return 0, errors.New("unexpected read after header")
	}
	return c.Reader.Read(p)
}

func requireHeaderRejection(t *testing.T, wire []byte, server bool, limit int, extensions []Extension, want error) {
	t.Helper()
	raw := &headerOnlyConn{memoryConn: memoryConn{Reader: bytes.NewReader(wire)}}
	c := NewConn(raw, server, extensions, WithMaxBytes(limit))
	_, _, err := c.ReadMessage()
	if !errors.Is(err, want) || raw.bodyReads != 0 {
		t.Fatalf("ReadMessage = %v, reads after header = %d; want %v without reading a body", err, raw.bodyReads, want)
	}
}

func TestReadMessageRejectsHeaderBeforeBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wire   []byte
		server bool
		limit  int
		want   error
	}{
		{"frame limit", []byte{0x82, 3}, false, 2, ErrPayloadTooLarge},
		{"message at limit", []byte{0x02, 2, 'a', 'b', 0x80, 1}, false, 2, ErrPayloadTooLarge},
		{"message one remaining", []byte{0x02, 1, 'a', 0x80, 2}, false, 2, ErrPayloadTooLarge},
		{"message after ping", []byte{0x02, 2, 'a', 'b', 0x89, 1, 'p', 0x80, 1}, false, 2, ErrPayloadTooLarge},
		{"message after pong", []byte{0x02, 2, 'a', 'b', 0x8a, 0, 0x80, 1}, false, 2, ErrPayloadTooLarge},
		{"masked continuation before key", []byte{0x02, 0x82, 0, 0, 0, 0, 'a', 'b', 0x80, 0x81}, true, 2, ErrPayloadTooLarge},
		{"unexpected continuation", []byte{0x80, 125}, false, 0, ErrUnexpectedContinuation},
		{"new text during fragmentation", []byte{0x02, 0, 0x81, 125}, false, 0, ErrUnexpectedFrame},
		{"new binary during fragmentation", []byte{0x01, 0, 0x82, 125}, false, 0, ErrUnexpectedFrame},
		{"RSV1", []byte{0xc2, 125}, false, 0, ErrUnsupportedExtensions},
		{"RSV2", []byte{0xa2, 125}, false, 0, ErrUnsupportedExtensions},
		{"RSV3", []byte{0x92, 125}, false, 0, ErrUnsupportedExtensions},
		{"reserved opcode", []byte{0x83, 125}, false, 0, ErrInvalidOpcode},
		{"masked server", []byte{0x82, 0x81}, false, 0, ErrMaskedFrame},
		{"unmasked client", []byte{0x82, 1}, true, 0, ErrUnmaskedFrame},
		{"fragmented control", []byte{0x09, 1}, false, 0, ErrControlFrameFragment},
		{"oversized control", []byte{0x89, 126}, false, 0, ErrPayloadTooLarge},
		{"one byte close", []byte{0x88, 1}, false, 0, ErrInvalidFrame},
		{"nonminimal 16 bit", []byte{0x82, 126, 0, 125}, false, 0, ErrInvalidFrame},
		{"nonminimal 64 bit", []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 255, 255}, false, 0, ErrInvalidFrame},
		{"high bit length", []byte{0x82, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}, false, 0, ErrInvalidFrame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireHeaderRejection(t, tc.wire, tc.server, tc.limit, nil, tc.want)
		})
	}
}

func TestReadMessageRemainingBudgetLengthBoundaries(t *testing.T) {
	for _, size := range []int{125, 126, 65535, 65536} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			var header []byte
			switch {
			case size <= 125:
				header = []byte{0x80, byte(size)}
			case size <= 65535:
				header = make([]byte, 4)
				header[0], header[1] = 0x80, 126
				binary.BigEndian.PutUint16(header[2:], uint16(size))
			default:
				header = make([]byte, 10)
				header[0], header[1] = 0x80, 127
				binary.BigEndian.PutUint64(header[2:], uint64(size))
			}
			// Each frame fits the configured cap individually, but the second
			// header proves that the reassembled message would exceed it.
			wire := append([]byte{0x02, 1, 'a'}, header...)
			requireHeaderRejection(t, wire, false, size, nil, ErrPayloadTooLarge)
			wire = append(wire, bytes.Repeat([]byte{'b'}, size)...)
			for _, limit := range []int{0, size + 1, size + 2} {
				_, data, err := frameConn(wire, false, limit).ReadMessage()
				if err != nil || len(data) != size+1 || data[0] != 'a' {
					t.Fatalf("limit %d: len = %d, error = %v", limit, len(data), err)
				}
			}
		})
	}
	maxInt := uint64(^uint(0) >> 1)
	header := make([]byte, 10)
	header[0], header[1] = 0x80, 127
	binary.BigEndian.PutUint64(header[2:], maxInt)
	requireHeaderRejection(t, append([]byte{0x02, 1, 'a'}, header...), false, int(maxInt), nil, ErrPayloadTooLarge)
	if strconv.IntSize == 32 {
		header[0] = 0x82
		binary.BigEndian.PutUint64(header[2:], maxInt+1)
		requireHeaderRejection(t, header, false, 0, nil, ErrPayloadTooLarge)
	}
}

func TestReadMessageControlsAtLimit(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, opcode := range []Opcode{PingMessage, PongMessage, CloseMessage} {
			t.Run(strconv.FormatBool(server)+"/"+opcode.String(), func(t *testing.T) {
				// A nonzero mask exercises payload handling independently from
				// the cumulative budget, including a valid empty final frame.
				frame := func(b0 byte, data []byte) []byte {
					wire := []byte{b0, byte(len(data))}
					if !server {
						return append(wire, data...)
					}
					wire[1] |= 0x80
					key := []byte{1, 2, 3, 4}
					wire = append(wire, key...)
					for i, b := range data {
						wire = append(wire, b^key[i%4])
					}
					return wire
				}
				control := bytes.Repeat([]byte{'p'}, 125)
				if opcode == CloseMessage {
					control[0], control[1] = 3, 232
				}
				wire := frame(0x02, []byte("ab"))
				wire = append(wire, frame(0x80|byte(opcode), control)...)
				wire = append(wire, frame(0x80, nil)...)
				c := frameConn(wire, server, 2)
				var received string
				c.SetPingHandler(func(s string) error { received = s; return nil })
				c.SetPongHandler(func(s string) error { received = s; return nil })
				op, data, err := c.ReadMessage()
				if opcode == CloseMessage {
					if err != io.EOF {
						t.Fatalf("close = %v", err)
					}
				} else if err != nil || op != BinaryMessage || string(data) != "ab" || received != string(control) {
					t.Fatalf("message = %v %q %v; control = %q", op, data, err, received)
				}
			})
		}
	}
}

// Incoming extensions are trusted transforms; even Opcode and Payload may
// change. Header-only rejection must not guess their decoded representation.
type headerTransformExtension struct {
	Extension
	enabled bool
	process func(*Frame) error
}

func (*headerTransformExtension) Name() string                          { return "header-transform" }
func (e *headerTransformExtension) IsEnabled() bool                     { return e.enabled }
func (e *headerTransformExtension) ProcessIncomingFrame(f *Frame) error { return e.process(f) }

func TestReadMessageCustomHeaderTransforms(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wire    []byte
		process func(*Frame) error
		want    string
		wantErr error
	}{
		{"shrinking continuation", []byte{0x02, 2, 'a', 'b', 0x80, 2, 'c', 'd'}, func(f *Frame) error {
			if f.Opcode == ContinuationFrame {
				f.Payload = nil
			}
			return nil
		}, "ab", nil},
		{"shrinking first fragment", []byte{0x02, 2, 'a', 'b', 0x80, 2, 'c', 'd'}, func(f *Frame) error {
			if f.Opcode == BinaryMessage {
				f.Payload = nil
			}
			return nil
		}, "cd", nil},
		{"normalize fragmented opcode", []byte{0x02, 0, 0x82, 1, 'x'}, func(f *Frame) error {
			if f.Final {
				f.Opcode = ContinuationFrame
			}
			return nil
		}, "x", nil},
		{"noop new message", []byte{0x02, 0, 0x82, 1, 'x'}, func(*Frame) error { return nil }, "", ErrUnexpectedFrame},
		{"reserved bits", []byte{0xf2, 1, 'x'}, func(f *Frame) error {
			f.Rsv1, f.Rsv2, f.Rsv3 = false, false, false
			return nil
		}, "x", nil},
		{"normalize opcode", []byte{0x80, 1, 'x'}, func(f *Frame) error {
			f.Opcode = BinaryMessage
			return nil
		}, "x", nil},
		{"normalize close", []byte{0x88, 1, 'x'}, func(f *Frame) error {
			f.Opcode = BinaryMessage
			return nil
		}, "x", nil},
		{"decoded overflow", []byte{0x82, 1, 'x'}, func(f *Frame) error {
			f.Payload = []byte("abc")
			return nil
		}, "", ErrPayloadTooLarge},
		{"noop overflow", []byte{0x02, 2, 'a', 'b', 0x80, 1, 'c'}, func(*Frame) error { return nil }, "", ErrPayloadTooLarge},
		{"noop continuation", []byte{0x80, 1, 'x'}, func(*Frame) error { return nil }, "", ErrUnexpectedContinuation},
		{"noop reserved bit", []byte{0xc2, 1, 'x'}, func(*Frame) error { return nil }, "", ErrUnsupportedExtensions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := frameConn(tc.wire, false, 2)
			c.extensions = []Extension{&headerTransformExtension{enabled: true, process: tc.process}}
			_, data, err := c.ReadMessage()
			if !errors.Is(err, tc.wantErr) || string(data) != tc.want {
				t.Fatalf("message = %q, %v; want %q, %v", data, err, tc.want, tc.wantErr)
			}
		})
	}
	disabled := []Extension{&headerTransformExtension{process: func(*Frame) error { t.Fatal("disabled extension invoked"); return nil }}}
	requireHeaderRejection(t, []byte{0xc2, 125}, false, 0, disabled, ErrUnsupportedExtensions)
	requireHeaderRejection(t, []byte{0x02, 2, 'a', 'b', 0x80, 1}, false, 2, disabled, ErrPayloadTooLarge)
}

func TestReadMessageBuiltinReservedBits(t *testing.T) {
	pmd := NewPerMessageDeflateExtension()
	if err := pmd.Negotiate("permessage-deflate"); err != nil {
		t.Fatal(err)
	}
	for _, b0 := range []byte{0xa2, 0x92, 0xe2, 0xf2, 0xc0, 0xc8, 0xc9, 0xca} {
		requireHeaderRejection(t, []byte{b0, 125}, false, 0, []Extension{pmd}, ErrUnsupportedExtensions)
	}
	// A custom extension may consume RSV2 before or after built-in processing.
	for _, first := range []bool{false, true} {
		custom := &headerTransformExtension{enabled: true, process: func(f *Frame) error { f.Rsv2 = false; return nil }}
		extensions := []Extension{pmd, custom}
		if first {
			extensions[0], extensions[1] = extensions[1], extensions[0]
		}
		c := frameConn([]byte{0xa2, 1, 'x'}, false, 1)
		c.extensions = extensions
		if _, data, err := c.ReadMessage(); err != nil || string(data) != "x" {
			t.Fatalf("custom first %v: %q, %v", first, data, err)
		}
	}
	// Enabled built-in compression does not transform continuation frames.
	requireHeaderRejection(t, []byte{0x02, 2, 'a', 'b', 0x80, 1}, false, 2, []Extension{pmd}, ErrPayloadTooLarge)
}

func TestReadMessageAtLimitStillRequiresFinalFrame(t *testing.T) {
	for _, tail := range [][]byte{nil, {0x80}, {0x80, 1}, {0x80, 126, 0}, {0x8a, 0}, {0x00, 0}} {
		wire := append([]byte{0x02, 2, 'a', 'b'}, tail...)
		_, _, err := frameConn(wire, false, 2).ReadMessage()
		want := io.ErrUnexpectedEOF
		if bytes.Equal(tail, []byte{0x80, 1}) {
			want = ErrPayloadTooLarge
		}
		if !errors.Is(err, want) {
			t.Fatalf("tail %x: %v, want %v", tail, err, want)
		}
	}
}

// stalledFrameConn lets a read reach each header/body boundary deterministically
// before Close interrupts it, with no sleeps or network scheduling assumptions.
type stalledFrameConn struct {
	memoryConn
	waiting, closed     chan struct{}
	waitOnce, closeOnce sync.Once
}

func (c *stalledFrameConn) Read(p []byte) (int, error) {
	if c.Reader.Len() > 0 {
		return c.Reader.Read(p)
	}
	c.waitOnce.Do(func() { close(c.waiting) })
	<-c.closed
	return 0, io.ErrClosedPipe
}

func (c *stalledFrameConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestCloseInterruptsFrameReadStages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wire   []byte
		server bool
	}{
		{"header", nil, false},
		{"partial header", []byte{0x82}, false},
		{"extended length", []byte{0x82, 126, 0}, false},
		{"mask key", []byte{0x82, 0x81, 1, 2}, true},
		{"payload", []byte{0x82, 2, 'x'}, false},
		{"continuation header at limit", []byte{0x02, 2, 'a', 'b', 0x80}, false},
		{"continuation payload", []byte{0x02, 1, 'a', 0x80, 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := &stalledFrameConn{memoryConn: memoryConn{Reader: bytes.NewReader(tc.wire)}, waiting: make(chan struct{}), closed: make(chan struct{})}
			defer raw.Close()
			c := NewConn(raw, tc.server, nil, WithMaxBytes(2))
			result := make(chan error, 1)
			go func() { _, _, err := c.ReadMessage(); result <- err }()
			select {
			case <-raw.waiting:
			case err := <-result:
				t.Fatalf("read returned before reaching expected boundary: %v", err)
			case <-time.After(time.Second):
				t.Fatal("read did not reach expected boundary")
			}
			closed := make(chan error, 1)
			go func() { closed <- c.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close waited for the stalled read")
			}
			select {
			case err := <-result:
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("read = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close did not interrupt frame read")
			}
		})
	}
}
