package websocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// memoryConn keeps malformed input tests deterministic and off the network.
type memoryConn struct {
	*bytes.Reader
	written bytes.Buffer
}

func (c *memoryConn) Write(p []byte) (int, error)    { return c.written.Write(p) }
func (*memoryConn) Close() error                     { return nil }
func (*memoryConn) LocalAddr() net.Addr              { return nil }
func (*memoryConn) RemoteAddr() net.Addr             { return nil }
func (*memoryConn) SetDeadline(time.Time) error      { return nil }
func (*memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (*memoryConn) SetWriteDeadline(time.Time) error { return nil }

// frameConn reads wire with a message limit of limit bytes, or none when limit
// is 0.
func frameConn(wire []byte, server bool, limit int) *Conn {
	if limit == 0 {
		limit = -1
	}
	return newTestConn(&memoryConn{Reader: bytes.NewReader(wire)}, ConnConfig{Server: server, MaxMessageSize: limit})
}

func TestReadFrameRejectsMalformedHeaders(t *testing.T) {
	tests := []struct {
		name   string
		wire   []byte
		server bool
		limit  int
		want   error
	}{
		{"high bit length", []byte{0x82, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}, false, 0, errInvalidFrame},
		{"nonminimal 16 bit", []byte{0x82, 126, 0, 0}, false, 0, errInvalidFrame},
		{"nonminimal 64 bit", []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 0, 126}, false, 0, errInvalidFrame},
		{"limit before payload", []byte{0x82, 126, 0x10, 0}, false, 32, ErrMessageTooLarge},
		{"masked server", []byte{0x82, 0x80, 0, 0, 0, 0}, false, 0, errMaskedFrame},
		{"unmasked client", []byte{0x82, 0}, true, 0, errUnmaskedFrame},
		{"reserved opcode", []byte{0x83, 0}, false, 0, errInvalidOpcode},
		{"rsv without extension", []byte{0xC2, 0}, false, 0, errUnsupportedExtensions},
		{"fragmented ping", []byte{0x09, 0}, false, 0, errControlFrameFragment},
		{"oversized ping", []byte{0x89, 126}, false, 0, errInvalidFrame},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if got := recover(); got != nil {
					t.Fatalf("readFrame panicked: %v", got)
				}
			}()
			_, err := frameConn(tt.wire, tt.server, tt.limit).readFrame()
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestReadMessageFragmentation(t *testing.T) {
	tests := []struct {
		name    string
		wire    []byte
		limit   int
		want    string
		wantErr error
	}{
		{"valid", []byte{0x01, 1, 'a', 0x80, 1, 'b'}, 2, "ab", nil},
		{"unexpected continuation", []byte{0x80, 1, 'a'}, 2, "", errUnexpectedContinuation},
		{"new message during fragmentation", []byte{0x01, 1, 'a', 0x81, 1, 'b'}, 2, "", errUnexpectedFrame},
		{"continuation exceeds limit", []byte{0x01, 1, 'a', 0x80, 2, 'b', 'c'}, 2, "", ErrMessageTooLarge},
		{"split utf8", []byte{0x01, 1, 0xC2, 0x80, 1, 0xA2}, 2, "¢", nil},
		{"invalid utf8", []byte{0x81, 1, 0xFF}, 2, "", errInvalidFrame},
		{"interleaved ping", []byte{0x01, 1, 'a', 0x89, 1, 'p', 0x80, 1, 'b'}, 2, "ab", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opcode, payload, err := frameConn(tt.wire, false, tt.limit).ReadMessage()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && (opcode != TextMessage || string(payload) != tt.want) {
				t.Fatalf("message = (%v, %q), want text %q", opcode, payload, tt.want)
			}
		})
	}
}

func TestReadFrameLengthBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 125, 126, 65535, 65536} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			raw := &memoryConn{Reader: bytes.NewReader(nil)}
			wire := &raw.written
			writer := newTestConn(raw, ConnConfig{Server: true})
			data := bytes.Repeat([]byte{'x'}, n)
			if err := writer.WriteMessage(BinaryMessage, data); err != nil {
				t.Fatal(err)
			}
			got, err := frameConn(wire.Bytes(), false, n+1).readFrame()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Payload, data) {
				t.Fatal("payload did not round trip")
			}
		})
	}
}

func TestReadFrameArchitectureLimit(t *testing.T) {
	if ^uint(0)>>63 != 0 {
		t.Skip("32-bit integer overflow case")
	}
	wire := make([]byte, 10)
	wire[0], wire[1] = 0x82, 127
	binary.BigEndian.PutUint64(wire[2:], 1<<32)
	_, err := frameConn(wire, false, 0).readFrame()
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("error = %v, want ErrMessageTooLarge", err)
	}
}

func FuzzReadMessage(f *testing.F) {
	for _, wire := range [][]byte{{0x81, 1, 'x'}, {0x01, 1, 'a', 0x80, 1, 'b'}, {0x82, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}, {0x89, 0, 0x81, 0}, {0x88, 0}} {
		f.Add(wire, false)
	}
	f.Add([]byte{0x82, 0x80, 0, 0, 0, 0}, true)
	f.Add([]byte{0xc2, 125}, false)
	f.Add([]byte{0x02, 0, 0x82, 125}, false)
	atLimit := append([]byte{0x02, 126, 4, 0}, bytes.Repeat([]byte{'a'}, 1024)...)
	f.Add(append(atLimit, 0x8a, 0, 0x80, 1), false)
	f.Fuzz(func(t *testing.T, wire []byte, server bool) {
		c := frameConn(wire, server, 1024)
		_, data, err := c.ReadMessage()
		if err == nil && len(data) > 1024 {
			t.Fatalf("limit bypass: %d", len(data))
		}
	})
}

var _ io.ReadWriter = (*memoryConn)(nil)

func TestClosePayloadValidation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		payload []byte
		valid   bool
	}{
		{"empty", nil, true}, {"normal", []byte{3, 232}, true}, {"reason", []byte{3, 232, 'b', 'y', 'e'}, true},
		{"application", []byte{15, 160}, true}, {"short", []byte{3}, false},
		{"reserved", []byte{3, 237}, false}, {"undefined", []byte{7, 208}, false},
		{"out of range", []byte{19, 136}, false}, {"invalid reason", []byte{3, 232, 0xFF}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := frameConn(append([]byte{0x88, byte(len(tt.payload))}, tt.payload...), false, 0)
			_, _, err := c.ReadMessage()
			if tt.valid {
				if CloseStatus(err) == -1 {
					t.Fatalf("error = %v, want a *CloseError", err)
				}
			} else if !errors.Is(err, errInvalidFrame) {
				t.Fatalf("error = %v, want errInvalidFrame", err)
			}
			c = frameConn(nil, true, 0)
			err = c.writeControlFrame(opClose, tt.payload)
			if tt.valid != (err == nil) {
				t.Fatalf("write error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestWriteRejectsInvalidOpcodesAndPayload(t *testing.T) {
	c := frameConn(nil, true, 0)
	for _, typ := range []MessageType{0, 3, 8, 9, 10, 16} {
		if !errors.Is(c.WriteMessage(typ, nil), errInvalidMessageType) {
			t.Fatalf("WriteMessage accepted %v", typ)
		}
	}
	for _, opcode := range []opcode{opContinuation, opText, opBinary, 3, 16} {
		if !errors.Is(c.writeControlFrame(opcode, nil), errInvalidOpcode) {
			t.Fatalf("writeControlFrame accepted %v", opcode)
		}
	}
	if !errors.Is(c.WriteMessage(TextMessage, []byte{0xff}), errInvalidUTF8) {
		t.Fatal("accepted invalid UTF-8")
	}
	if !errors.Is(c.writeControlFrame(opPing, make([]byte, 126)), errInvalidFrame) {
		t.Fatal("accepted oversized ping")
	}
}

func TestPerMessageDeflateRFCVector(t *testing.T) {
	// Independently specified bytes from RFC 7692 section 7.2.3.1.
	pmd := newPerMessageDeflate()
	if err := pmd.Negotiate("permessage-deflate; client_no_context_takeover; server_no_context_takeover"); err != nil {
		t.Fatal(err)
	}
	c := frameConn([]byte{0xc1, 7, 0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00}, false, 16)
	c.extensions = []frameExtension{pmd}
	_, data, err := c.ReadMessage()
	if err != nil || string(data) != "Hello" {
		t.Fatalf("message = %q, %v", data, err)
	}
}

func TestPerMessageDeflateBoundsDecodedSize(t *testing.T) {
	for _, limit := range []int{128, 129, 130} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			pmd := newPerMessageDeflate().(*perMessageDeflate)
			if err := pmd.Negotiate("permessage-deflate; client_no_context_takeover; server_no_context_takeover"); err != nil {
				t.Fatal(err)
			}
			frame := &wireFrame{Final: true, Opcode: opBinary, Payload: bytes.Repeat([]byte{'a'}, 129)}
			if err := pmd.ProcessOutgoingFrame(frame); err != nil {
				t.Fatal(err)
			}
			if len(frame.Payload) >= 128 {
				t.Fatal("fixture failed to compress")
			}
			c := frameConn(append([]byte{0xc2, byte(len(frame.Payload))}, frame.Payload...), false, limit)
			c.extensions = []frameExtension{pmd}
			_, data, err := c.ReadMessage()
			if limit < 129 {
				if !errors.Is(err, ErrMessageTooLarge) {
					t.Fatalf("error = %v, want ErrMessageTooLarge", err)
				}
			} else if err != nil || len(data) != 129 {
				t.Fatalf("message length = %d, error = %v", len(data), err)
			}
		})
	}
}

func TestReadFrameHugeDeclarationDoesNotAllocate(t *testing.T) {
	wire := make([]byte, 10)
	wire[0], wire[1] = 0x82, 127
	binary.BigEndian.PutUint64(wire[2:], uint64(^uint(0)>>1))
	// Only the header is present. A declaration alone must not allocate MaxInt.
	_, err := frameConn(wire, false, 0).readFrame()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
}

// newTestConn is NewConn for configurations that cannot fail, with
// extensions negotiated after cfg.Extensions'.
func newTestConn(conn net.Conn, cfg ConnConfig, extensions ...frameExtension) *Conn {
	c, err := newConfiguredConn(conn, cfg, extensions)
	if err != nil {
		panic(err)
	}
	return c
}

// dialExtensions is d.Dial with extensions offered after Compression's.
func dialExtensions(ctx context.Context, d *Dialer, urlStr string, extensions []frameExtension) (*Conn, *http.Response, error) {
	return d.dial(ctx, urlStr, extensions)
}

// upgradeExtensions is u.Upgrade with extensions negotiated after
// Compression's.
func upgradeExtensions(w http.ResponseWriter, r *http.Request, u *Upgrader, extensions []frameExtension) (*Conn, error) {
	return u.upgrade(w, r, extensions)
}
