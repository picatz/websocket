package websocket

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
)

// These tests exercise limits inherited from Dial and Upgrade, then install a
// per-connection extension to isolate the read path from negotiation (which has
// separate coverage). PMD inputs are independent, unfragmented messages without
// context takeover; this does not assert support for compressed fragmentation
// or retained compression history.
func defaultLimitPMD(t *testing.T, server bool) Extension {
	t.Helper()
	pmd := NewPerMessageDeflateExtension(WithClientNoContextTakeover(), WithServerNoContextTakeover()).(*perMessageDeflate)
	pmd.server = server
	if err := pmd.Negotiate("permessage-deflate; client_no_context_takeover; server_no_context_takeover"); err != nil {
		t.Fatal(err)
	}
	return pmd
}

// RFC 1951 section 3.2.4: byte-aligned, non-final stored blocks contain LEN,
// one's-complement NLEN, and literal data. RFC 7692 removes the four LEN/NLEN
// bytes of the empty sync-flush block, leaving its zero header byte on the wire.
func defaultStoredDeflate(payload []byte) []byte {
	var wire []byte
	for len(payload) > 0 {
		n := min(len(payload), 65535)
		wire = append(wire, 0)
		wire = binary.LittleEndian.AppendUint16(wire, uint16(n))
		wire = binary.LittleEndian.AppendUint16(wire, ^uint16(n))
		wire = append(wire, payload[:n]...)
		payload = payload[n:]
	}
	return append(wire, 0)
}

// Construct a fixed-Huffman DEFLATE block directly from RFC 1951 sections
// 3.2.5-3.2.6: literal 'a' has code 10010001, length 258 has code 11000101,
// distance 1 has code 00000, and end-of-block has code 0000000. Repeating the
// length/distance pair generates large decoded inputs without the production
// encoder or an opaque compressed fixture.
func defaultRepeatedDeflate(size int) []byte {
	var wire []byte
	var bits uint
	putBit := func(bit uint) {
		if bits%8 == 0 {
			wire = append(wire, 0)
		}
		wire[len(wire)-1] |= byte(bit&1) << (bits % 8)
		bits++
	}
	putCode := func(code uint, width int) {
		// Huffman codes are emitted most-significant bit first.
		for i := width - 1; i >= 0; i-- {
			putBit(code >> i)
		}
	}
	putBit(0) // BFINAL=0.
	putBit(1) // BTYPE=01 (fixed Huffman), least-significant bit first.
	putBit(0)
	if size > 0 {
		putCode(0x91, 8) // Establish the first 'a' for distance-one matches.
		size--
	}
	for size >= 258 {
		putCode(0xc5, 8)
		putCode(0, 5)
		size -= 258
	}
	for range size {
		putCode(0x91, 8)
	}
	putCode(0, 7) // End of the fixed-Huffman block.
	for range 3 {
		putBit(0) // Non-final empty stored block header for sync-flush.
	}
	for bits%8 != 0 {
		putBit(0)
	}
	return wire // Omit the empty block's 00 00 ff ff suffix, per RFC 7692.
}

func requireDefaultExtensionTerminal(t *testing.T, c *Conn, raw *memoryConn, closeCode uint16) {
	t.Helper()
	if _, p, err := c.ReadMessage(); !errors.Is(err, io.ErrClosedPipe) || p != nil {
		t.Fatalf("later read: payload %d, error %v", len(p), err)
	}
	if err := c.WriteMessage(BinaryMessage, []byte("later")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("later write: %v", err)
	}
	if c.closeSent != (closeCode != 0) {
		t.Fatalf("best-effort Close attempted = %v, want code %d", c.closeSent, closeCode)
	}
	// Upgrade's memory transport proves the exact notification. Dial's peer
	// may already be closed, so only the best-effort attempt is guaranteed.
	if raw != nil {
		if closeCode == 0 {
			if raw.written.Len() != 0 {
				t.Fatalf("unexpected automatic extension output: %x", raw.written.Bytes())
			}
		} else if p := failureClosePayload(t, raw.written.Bytes(), true); binary.BigEndian.Uint16(p) != closeCode {
			t.Fatalf("close payload = %x, want code %d", p, closeCode)
		}
	}
}

func TestHighLevelDefaultPMDBoundaries(t *testing.T) {
	n := DefaultMaxMessageSize
	// Sixteen stored blocks add 80 bytes, plus the retained sync-flush byte.
	storedAt := bytes.Repeat([]byte{'s'}, n-81)
	storedOver := bytes.Repeat([]byte{'s'}, n-80)
	cases := []struct {
		name          string
		encoded, want []byte
		overflow      bool
	}{
		// RFC 7692 section 7.2.3.1, independently specified "Hello" example.
		{"RFC Hello", []byte{0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00}, []byte("Hello"), false},
		{"encoded at limit", defaultStoredDeflate(storedAt), storedAt, false},
		{"encoded n+1 decoded below n", defaultStoredDeflate(storedOver), storedOver, true},
		{"decoded at limit", defaultRepeatedDeflate(n), bytes.Repeat([]byte{'a'}, n), false},
		{"decoded n+1 encoded below n", defaultRepeatedDeflate(n + 1), bytes.Repeat([]byte{'a'}, n+1), true},
	}
	if len(cases[1].encoded) != n || len(cases[2].encoded) != n+1 || len(cases[4].encoded) >= n {
		t.Fatal("independent vectors do not straddle the intended encoded boundaries")
	}
	for _, server := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("server=%v/%s", server, tc.name), func(t *testing.T) {
				c, raw := highLevelReader(t, server, limitWire(server, 0xc2, tc.encoded))
				c.extensions = []Extension{defaultLimitPMD(t, server)}
				if c.maxBytes != n {
					t.Fatalf("constructor limit = %d, want %d", c.maxBytes, n)
				}
				op, p, err := c.ReadMessage()
				if tc.overflow {
					if !errors.Is(err, ErrPayloadTooLarge) || p != nil {
						t.Fatalf("payload %d, error %v; want terminal size failure", len(p), err)
					}
					requireDefaultExtensionTerminal(t, c, raw, 1009)
					// An explicit larger limit proves this rejected input was
					// valid independently of the constructor's default budget.
					c, _ = highLevelReader(t, server, limitWire(server, 0xc2, tc.encoded), n+1)
					c.extensions = []Extension{defaultLimitPMD(t, server)}
					op, p, err = c.ReadMessage()
				}
				if err != nil || op != BinaryMessage || !bytes.Equal(p, tc.want) {
					t.Fatalf("read opcode %v, payload %d, error %v; want %d decoded bytes", op, len(p), err, len(tc.want))
				}
			})
		}
	}
}

func TestHighLevelDefaultPMDIncompressibleBoundary(t *testing.T) {
	// A fixed-seed xorshift stream supplies reproducible, high-entropy bytes.
	// Stored DEFLATE exposes the framing overhead explicitly: decoded data at
	// exactly 1 MiB can still exceed the independent encoded-frame limit.
	payload := make([]byte, DefaultMaxMessageSize)
	state := uint32(0x6d2b79f5)
	for i := range payload {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		payload[i] = byte(state)
	}
	encoded := defaultStoredDeflate(payload)
	if len(encoded) <= DefaultMaxMessageSize {
		t.Fatal("stored DEFLATE must include framing overhead")
	}
	for _, server := range []bool{false, true} {
		t.Run(fmt.Sprintf("server=%v", server), func(t *testing.T) {
			c, raw := highLevelReader(t, server, limitWire(server, 0xc2, encoded))
			c.extensions = []Extension{defaultLimitPMD(t, server)}
			if _, p, err := c.ReadMessage(); !errors.Is(err, ErrPayloadTooLarge) || p != nil {
				t.Fatalf("payload %d, error %v", len(p), err)
			}
			requireDefaultExtensionTerminal(t, c, raw, 1009)
			c, _ = highLevelReader(t, server, limitWire(server, 0xc2, encoded), len(encoded))
			c.extensions = []Extension{defaultLimitPMD(t, server)}
			if op, p, err := c.ReadMessage(); err != nil || op != BinaryMessage || !bytes.Equal(p, payload) {
				t.Fatalf("with encoded headroom: opcode %v, payload %d, error %v", op, len(p), err)
			}
		})
	}
}

func TestHighLevelDefaultPMDInvalidStreams(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, truncated := range []bool{false, true} {
			t.Run(fmt.Sprintf("server=%v/truncated=%v", server, truncated), func(t *testing.T) {
				encoded := []byte{0x06} // Reserved BTYPE=11, always malformed.
				if truncated {
					// Stored block declares 32 literal bytes but supplies only
					// one. Even all nine bytes appended by PMD cannot fill it.
					encoded = []byte{0x00, 0x20, 0x00, 0xdf, 0xff, 'x'}
				}
				c, raw := highLevelReader(t, server, limitWire(server, 0xc2, encoded))
				c.extensions = []Extension{defaultLimitPMD(t, server)}
				_, p, err := c.ReadMessage()
				if p != nil || err == nil || errors.Is(err, ErrPayloadTooLarge) {
					t.Fatalf("payload %d, error %v", len(p), err)
				}
				if truncated {
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("truncated DEFLATE: %v", err)
					}
				} else {
					var corrupt flate.CorruptInputError
					if !errors.As(err, &corrupt) {
						t.Fatalf("malformed DEFLATE: %v", err)
					}
				}
				requireDefaultExtensionTerminal(t, c, raw, 0)
			})
		}
	}
}

type defaultLimitExtension struct {
	process            func(*Frame) error
	incoming, outgoing int
}

func (*defaultLimitExtension) Name() string           { return "default-limit-test" }
func (*defaultLimitExtension) Offer() string          { return "default-limit-test" }
func (*defaultLimitExtension) Negotiate(string) error { return nil }
func (*defaultLimitExtension) IsEnabled() bool        { return true }
func (e *defaultLimitExtension) ProcessIncomingFrame(f *Frame) error {
	e.incoming++
	return e.process(f)
}
func (e *defaultLimitExtension) ProcessOutgoingFrame(*Frame) error {
	e.outgoing++
	return errors.New("automatic failure must not invoke a custom encoder")
}

func TestHighLevelDefaultExtensionHeaderLimit(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, custom := range []bool{false, true} {
			for _, size := range []int{DefaultMaxMessageSize + 1, int(^uint(0) >> 1)} {
				t.Run(fmt.Sprintf("server=%v/custom=%v/size=%d", server, custom, size), func(t *testing.T) {
					c, raw := highLevelReader(t, server, nil)
					ext := &defaultLimitExtension{process: func(f *Frame) error { f.Payload = nil; return nil }}
					c.extensions = []Extension{defaultLimitPMD(t, server)}
					code := uint16(1009)
					if custom {
						c.extensions, code = []Extension{ext}, 0
					}
					// Instrument only post-handshake input. A declaration must
					// reject before reading a body or even the client's mask,
					// including when an extension could shrink the payload.
					input := &headerOnlyConn{memoryConn: memoryConn{Reader: bytes.NewReader(limitHeader(server, 0xc2, size))}}
					c.rw.Reader = bufio.NewReader(input)
					if _, p, err := c.ReadMessage(); !errors.Is(err, ErrPayloadTooLarge) || p != nil || input.bodyReads != 0 || ext.incoming != 0 {
						t.Fatalf("payload %d, error %v, body reads %d, transforms %d", len(p), err, input.bodyReads, ext.incoming)
					}
					requireDefaultExtensionTerminal(t, c, raw, code)
					if ext.outgoing != 0 {
						t.Fatal("failure invoked custom encoder")
					}
				})
			}
		}
	}
}

func TestHighLevelDefaultCustomExtensionOutput(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, shrink := range []bool{false, true} {
			t.Run(fmt.Sprintf("server=%v/shrink=%v", server, shrink), func(t *testing.T) {
				// Custom extensions retain ownership of opcode, FIN, reserved
				// bits and intermediate storage. Only their final message output
				// is bounded; their own allocations remain their responsibility.
				ext := &defaultLimitExtension{process: func(f *Frame) error {
					f.Final, f.Opcode = true, BinaryMessage
					f.Rsv1, f.Rsv2, f.Rsv3 = false, false, false
					f.Payload = bytes.Repeat([]byte{'x'}, DefaultMaxMessageSize+1)
					return nil
				}}
				c, raw := highLevelReader(t, server, limitWire(server, 0x70, []byte("x")))
				c.extensions = []Extension{ext}
				trim := &defaultLimitExtension{process: func(f *Frame) error {
					f.Payload = f.Payload[:DefaultMaxMessageSize]
					return nil
				}}
				if shrink {
					c.extensions = append(c.extensions, trim)
				}
				op, p, err := c.ReadMessage()
				if shrink {
					if err != nil || op != BinaryMessage || !bytes.Equal(p, bytes.Repeat([]byte{'x'}, DefaultMaxMessageSize)) || trim.incoming != 1 {
						t.Fatalf("transformed output: opcode %v, payload %d, error %v, trims %d", op, len(p), err, trim.incoming)
					}
				} else {
					if !errors.Is(err, ErrPayloadTooLarge) || p != nil {
						t.Fatalf("payload %d, error %v", len(p), err)
					}
					requireDefaultExtensionTerminal(t, c, raw, 0)
				}
				if ext.incoming != 1 || ext.outgoing != 0 || trim.outgoing != 0 {
					t.Fatalf("callbacks: incoming %d, outgoing %d/%d", ext.incoming, ext.outgoing, trim.outgoing)
				}
			})
		}
	}
}

func TestHighLevelDefaultCustomExtensionError(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, cause := range []error{io.EOF, ErrPayloadTooLarge} {
			t.Run(fmt.Sprintf("server=%v/%v", server, cause), func(t *testing.T) {
				ext := &defaultLimitExtension{process: func(*Frame) error { return fmt.Errorf("custom cause: %w", cause) }}
				wire := limitWire(server, 0x82, []byte("first"))
				wire = append(wire, limitWire(server, 0x82, []byte("later"))...)
				c, raw := highLevelReader(t, server, wire)
				c.extensions = []Extension{ext}
				_, p, err := c.ReadMessage()
				want := "extension default-limit-test failed to process incoming frame: custom cause: " + cause.Error()
				if !errors.Is(err, cause) || err.Error() != want || p != nil {
					t.Fatalf("payload %d, error %v; want %q", len(p), err, want)
				}
				requireDefaultExtensionTerminal(t, c, raw, 0)
				if ext.incoming != 1 || ext.outgoing != 0 {
					t.Fatalf("callbacks: incoming %d, outgoing %d", ext.incoming, ext.outgoing)
				}
			})
		}
	}
}
