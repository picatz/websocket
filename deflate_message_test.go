package websocket

import (
	"bytes"
	"encoding/hex"
	"testing"
	"unicode/utf8"
)

func TestPMDMessageRFCFragmentation(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, encoded := range []string{"f248cdc9c90700", "000500faff48656c6c6f00", "f348cdc9c9070000", "f24805000000ffffcac9c90700"} {
			payload, _ := hex.DecodeString(encoded)
			for split := 0; split <= len(payload); split++ {
				p := NewPerMessageDeflateExtension().(*perMessageDeflate)
				p.enabled = true
				wire := failureWire(server, 0x41, payload[:split])
				wire = append(wire, failureWire(server, 0x89, []byte("ping"))...)
				wire = append(wire, failureWire(server, 0x80, payload[split:])...)
				wire = append(wire, failureWire(server, 0x81, []byte("next"))...)
				raw := &memoryConn{Reader: bytes.NewReader(wire)}
				c := NewConn(raw, server, []Extension{p})
				typ, got, err := c.ReadMessage()
				if err != nil || typ != TextMessage || string(got) != "Hello" {
					t.Fatalf("server=%v payload=%s split=%d: %v %q", server, encoded, split, err, got)
				}
				_, got, err = c.ReadMessage()
				if err != nil || string(got) != "next" {
					t.Fatalf("next: %q %v", got, err)
				}
			}
		}
	}
}
func TestPMDMessageTakeoverAndFinalStreams(t *testing.T) {
	for _, server := range []bool{false, true} {
		p := NewPerMessageDeflateExtension().(*perMessageDeflate)
		p.enabled = true
		first, _ := hex.DecodeString("f248cdc9c90700")
		second, _ := hex.DecodeString("f200110000")
		finals, _ := hex.DecodeString("f348cdc9c90700f348cdc9c9070000")
		var wire []byte
		for _, payload := range [][]byte{first, second, finals} {
			wire = append(wire, failureWire(server, 0xc1, payload)...)
		}
		c := NewConn(&memoryConn{Reader: bytes.NewReader(wire)}, server, []Extension{p})
		for _, want := range []string{"Hello", "Hello", "HelloHello"} {
			_, got, err := c.ReadMessage()
			if err != nil || string(got) != want {
				t.Fatalf("server=%v %q %v", server, got, err)
			}
		}
		if p.receive == nil || len(p.receive.history) != 20 {
			t.Fatal("history was not committed")
		}
	}
}

// FuzzPMDReadMessage drives arbitrary wire bytes through ReadMessage with
// permessage-deflate and context takeover enabled. Decoded messages must stay
// within the configured limit, and text messages must be valid UTF-8.
func FuzzPMDReadMessage(f *testing.F) {
	hello := []byte{0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00}
	history := []byte{0xf2, 0x00, 0x11, 0x00, 0x00}
	f.Add(append(failureWire(false, 0xc1, hello), failureWire(false, 0xc2, history)...), false)
	f.Add(append(failureWire(true, 0x41, hello[:3]), failureWire(true, 0x80, hello[3:])...), true)
	f.Add(failureWire(false, 0xc2, []byte{0x06}), false)
	f.Add(failureWire(false, 0xc2, bytes.Repeat([]byte{0x00}, 64)), false)
	const limit = 1024
	f.Fuzz(func(t *testing.T, wire []byte, server bool) {
		p := NewPerMessageDeflateExtension().(*perMessageDeflate)
		p.server = server
		if err := p.Negotiate("permessage-deflate"); err != nil {
			t.Fatal(err)
		}
		c := NewConn(&memoryConn{Reader: bytes.NewReader(wire)}, server, []Extension{p}, WithMaxBytes(limit))
		for range 4 {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if len(data) > limit {
				t.Fatalf("limit bypass: %d", len(data))
			}
			if kind == TextMessage && !utf8.Valid(data) {
				t.Fatalf("invalid UTF-8 text message: %q", data)
			}
		}
	})
}
