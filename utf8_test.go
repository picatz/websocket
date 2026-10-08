package websocket

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
	"unicode/utf8"
)

// Check the streaming result against a rune-by-rune standard-library oracle.
func referenceUTF8Prefix(p []byte) (int, bool) {
	for i := 0; i < len(p); {
		r, n := utf8.DecodeRune(p[i:])
		if r == utf8.RuneError && n == 1 {
			return i, !utf8.FullRune(p[i:])
		}
		i += n
	}
	return len(p), true
}

func TestValidUTF8Prefix(t *testing.T) {
	for _, p := range [][]byte{
		nil, {}, {'a'}, {0}, {0x7f}, {0xc2}, {0xc2, 0x80},
		{0xe0}, {0xe0, 0xa0}, {0xe0, 0xa0, 0x80}, {0xed, 0x9f, 0xbf},
		{0xee, 0x80, 0x80}, {0xef, 0xbf, 0xbd}, {0xef, 0xbf, 0xbf},
		{0xf0}, {0xf0, 0x90}, {0xf0, 0x90, 0x80}, {0xf0, 0x90, 0x80, 0x80}, {0xf4, 0x8f, 0xbf, 0xbf},
		{0x80}, {0xbf}, {0xc0}, {0xc1}, {0xf5}, {0xf8}, {0xff},
		{0xc2, 'a'}, {0xe0, 0x9f}, {0xed, 0xa0}, {0xf0, 0x8f}, {0xf4, 0x90},
		{0xe1, 0x80, 'a'}, {0xf1, 0x80, 0x80, 'a'},
		{0x80, 0x80, 0x80, 0x80, 0xc2}, {0xff, 0xf0, 0x90},
	} {
		for _, prefix := range [][]byte{nil, []byte("ASCII prefix"), []byte("¢€𐀀")} {
			p := append(bytes.Clone(prefix), p...)
			got, valid := validUTF8Prefix(p)
			want, wantValid := referenceUTF8Prefix(p)
			if valid != wantValid || (valid && (got != want || len(p)-got >= utf8.UTFMax)) {
				t.Fatalf("prefix %x = %d, %v; want %d, %v", p, got, valid, want, wantValid)
			}
		}
	}
}

func TestValidUTF8PrefixAllScalars(t *testing.T) {
	var p [utf8.UTFMax]byte
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		n := utf8.EncodeRune(p[:], r)
		for split := 0; split <= n; split++ {
			got, valid := validUTF8Prefix(p[:split])
			want := 0
			if split == n {
				want = n
			}
			if !valid || got != want {
				t.Fatalf("rune %U split %d = %d, %v; want %d, true", r, split, got, valid, want)
			}
		}
	}
}

func TestReadMessageUTF8Splits(t *testing.T) {
	// Include boundary scalars, a literal replacement character, NUL and noncharacters.
	// Every two-split boundary includes empty frames and controls between rune bytes.
	payload := []byte("\x00\x7f\u0080\u07ff\u0800\ud7ff\ue000\ufffd\uffff\U00010000\U0010ffff")
	for _, server := range []bool{false, true} {
		for a := 0; a <= len(payload); a++ {
			for b := a; b <= len(payload); b++ {
				wire := failureWire(server, 0x01, payload[:a])
				wire = append(wire, failureWire(server, 0x89, []byte{0xff})...)
				wire = append(wire, failureWire(server, 0x00, nil)...)
				wire = append(wire, failureWire(server, 0x00, payload[a:b])...)
				wire = append(wire, failureWire(server, 0x8a, []byte{0xff})...)
				wire = append(wire, failureWire(server, 0x80, payload[b:])...)
				wire = append(wire, failureWire(server, 0x81, []byte("next"))...)
				c := frameConn(wire, server, len(payload))
				ping, pong := 0, 0
				c.SetPingHandler(func(string) error { ping++; return nil })
				c.SetPongHandler(func(string) error { pong++; return nil })
				op, got, err := c.ReadMessage()
				if err != nil || op != TextMessage || !bytes.Equal(got, payload) || ping != 1 || pong != 1 {
					t.Fatalf("role %v splits %d/%d = %v %x %v; controls %d/%d", server, a, b, op, got, err, ping, pong)
				}
				if op, got, err = c.ReadMessage(); err != nil || op != TextMessage || string(got) != "next" {
					t.Fatalf("next message = %v %q %v", op, got, err)
				}
			}
		}
	}
}

func TestReadMessageUTF8RejectsBeforeFIN(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, invalid := range [][]byte{
			{0x80}, {0xc0}, {0xc1}, {0xf5}, {0xff}, {0xc2, 'x'}, {0xe0, 0x9f}, {0xed, 0xa0}, {0xf0, 0x8f}, {0xf4, 0x90},
			{0xe1, 0x80, 'x'}, {0xf1, 0x80, 0x80, 'x'},
		} {
			for split := 0; split <= len(invalid); split++ {
				t.Run(fmt.Sprintf("server=%v/%x/split=%d", server, invalid, split), func(t *testing.T) {
					// EOF would otherwise return io.ErrUnexpectedEOF, not ErrInvalidFrame.
					wire := failureWire(server, 0x01, append([]byte("ok"), invalid[:split]...))
					wire = append(wire, failureWire(server, 0x8a, nil)...)
					wire = append(wire, failureWire(server, 0x00, invalid[split:])...)
					raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(wire)}}
					c := NewConn(raw, server, nil)
					op, p, err := c.ReadMessage()
					if op != 0 || p != nil || err != ErrInvalidFrame {
						t.Fatalf("read = %v %x %v", op, p, err)
					}
					if code := binary.BigEndian.Uint16(failureClosePayload(t, raw.written.Bytes(), server)); code != 1007 {
						t.Fatalf("close code %d", code)
					}
					before := bytes.Clone(raw.written.Bytes())
					if _, _, err := c.ReadMessage(); err != io.ErrClosedPipe {
						t.Fatal(err)
					}
					if err := c.WriteMessage(TextMessage, []byte("late")); err != io.ErrClosedPipe {
						t.Fatal(err)
					}
					if raw.closes != 1 || !bytes.Equal(raw.written.Bytes(), before) {
						t.Fatal("failure was not terminal")
					}
				})
			}
		}
	}
}

func TestReadMessageUTF8RejectsWhilePeerWaits(t *testing.T) {
	for _, server := range []bool{false, true} {
		t.Run(fmt.Sprintf("server=%v", server), func(t *testing.T) {
			raw, peer := net.Pipe()
			defer raw.Close()
			defer peer.Close()
			peer.SetDeadline(time.Now().Add(3 * time.Second))
			c := NewConn(raw, server, nil)
			done := make(chan error, 1)
			go func() { _, _, err := c.ReadMessage(); done <- err }()
			// ED is incomplete; A0 proves a surrogate without a final frame.
			for _, wire := range [][]byte{failureWire(server, 0x01, []byte{0xed}), failureWire(server, 0x00, []byte{0xa0})} {
				if _, err := peer.Write(wire); err != nil {
					t.Fatal(err)
				}
			}
			wire, err := io.ReadAll(peer)
			if err != nil {
				t.Fatal(err)
			}
			if code := binary.BigEndian.Uint16(failureClosePayload(t, wire, server)); code != 1007 {
				t.Fatalf("code %d", code)
			}
			if err := <-done; err != ErrInvalidFrame {
				t.Fatal(err)
			}
		})
	}
}

func TestReadMessageUTF8IncompleteAndBinary(t *testing.T) {
	for _, p := range [][]byte{{0xc2}, {0xe0}, {0xe0, 0xa0}, {0xf0}, {0xf0, 0x90}, {0xf0, 0x90, 0x80}} {
		for _, final := range []bool{false, true} {
			wire := failureWire(false, 0x01, p)
			if final {
				wire = append(wire, failureWire(false, 0x80, nil)...)
			}
			_, _, err := frameConn(wire, false, 0).ReadMessage()
			want := io.ErrUnexpectedEOF
			if final {
				want = ErrInvalidFrame
			}
			if err != want {
				t.Fatalf("%x final=%v: %v, want %v", p, final, err, want)
			}
		}
	}
	for _, server := range []bool{false, true} {
		wire := failureWire(server, 0x02, []byte{0xff, 0xed})
		wire = append(wire, failureWire(server, 0x80, []byte{0xa0})...)
		op, p, err := frameConn(wire, server, 3).ReadMessage()
		if err != nil || op != BinaryMessage || !bytes.Equal(p, []byte{0xff, 0xed, 0xa0}) {
			t.Fatalf("binary = %v %x %v", op, p, err)
		}
	}
}

func TestReadMessageUTF8AfterExtensions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wire      []byte
		transform func(*Frame) error
		want      []byte
		wantErr   error
	}{
		{"decode before validation", []byte{0x01, 1, 0xff, 0x80, 1, 0xff}, func(f *Frame) error {
			if f.Opcode == TextMessage {
				f.Payload = []byte{0xc2}
			} else {
				f.Payload = []byte{0xa2}
			}
			return nil
		}, []byte("¢"), nil},
		{"decoded invalid before FIN", []byte{0x01, 1, 'x'}, func(f *Frame) error { f.Payload = []byte{0xed, 0xa0}; return nil }, nil, ErrInvalidFrame},
		{"normalize opcode", []byte{0x02, 1, 0xff}, func(f *Frame) error { f.Opcode = TextMessage; return nil }, nil, ErrInvalidFrame},
		{"normalize final", []byte{0x01, 1, 0xc2}, func(f *Frame) error { f.Final = true; return nil }, nil, ErrInvalidFrame},
		{"decoded limit first", []byte{0x01, 1, 'x'}, func(f *Frame) error { f.Payload = []byte{0xff, 0xff, 0xff}; return nil }, nil, ErrPayloadTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := frameConn(tc.wire, false, 2)
			c.extensions = []Extension{&headerTransformExtension{enabled: true, process: tc.transform}}
			_, p, err := c.ReadMessage()
			if err != tc.wantErr || !bytes.Equal(p, tc.want) {
				t.Fatalf("read = %x %v; want %x %v", p, err, tc.want, tc.wantErr)
			}
		})
	}
}

func FuzzUTF8Fragments(f *testing.F) {
	for _, p := range [][]byte{nil, []byte("ASCII"), []byte("a¢€𐀀�"), {0xe0, 0x9f}, {0xed, 0xa0}, {0xf0, 0x8f}, {0xf4, 0x90}, {0xc0, 0x80}, {0xf1, 0x80, 0x80, 0x80}, {0xff, 0xc2}} {
		for _, chunks := range [][]byte{{0}, {1}, {2}, {3}, {1, 2, 0, 3}} {
			f.Add(p, chunks, false)
		}
	}
	f.Fuzz(func(t *testing.T, p, chunks []byte, server bool) {
		if len(p) > 4096 || len(chunks) > 128 {
			t.Skip()
		}
		validated := 0
		var wire []byte
		start, part := 0, 0
		for {
			size := 1
			if len(chunks) > 0 {
				size = 1 + int(chunks[part%len(chunks)]%64)
			}
			end := min(len(p), start+size)
			first := byte(ContinuationFrame)
			if part == 0 {
				first = byte(TextMessage)
			}
			if end == len(p) {
				first |= 0x80
			}
			wire = append(wire, failureWire(server, first, p[start:end])...)
			if validated >= 0 {
				n, valid := validUTF8Prefix(p[validated:end])
				want, wantValid := referenceUTF8Prefix(p[:end])
				if valid != wantValid || (valid && (validated+n != want || end-validated-n >= utf8.UTFMax)) {
					t.Fatalf("prefix %x: incremental %d+%d/%v, oracle %d/%v", p[:end], validated, n, valid, want, wantValid)
				}
				if valid {
					validated += n
				} else {
					validated = -1
				}
			}
			if end == len(p) {
				break
			}
			wire = append(wire, failureWire(server, 0x8a, []byte{0xff})...)
			start = end
			part++
		}
		op, data, err := frameConn(wire, server, len(p)+1).ReadMessage()
		if utf8.Valid(p) {
			if err != nil || op != TextMessage || !bytes.Equal(data, p) {
				t.Fatalf("valid: %v %x %v", op, data, err)
			}
		} else if err != ErrInvalidFrame || data != nil || op != 0 {
			t.Fatalf("invalid: %v %x %v", op, data, err)
		}
	})
}

func TestValidUTF8PrefixAllocations(t *testing.T) {
	payload := bytes.Repeat([]byte("a¢€𐀀"), 100)
	if got := testing.AllocsPerRun(100, func() {
		validated := 0
		for end := 1; end <= len(payload); end++ {
			n, valid := validUTF8Prefix(payload[validated:end])
			if !valid {
				panic("valid UTF-8 rejected")
			}
			validated += n
		}
	}); got != 0 {
		t.Fatalf("validator allocations = %g, want 0", got)
	}
}
