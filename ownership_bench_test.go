package websocket

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

var ownershipBenchmarkResult []byte

// Include exact-capacity candidates and adjacent spare-capacity controls.
// Eligibility depends on the actual frame buffer, not a fixed size threshold.
func BenchmarkReadMessageOwnership(b *testing.B) {
	for _, size := range []int{0, 1, 16, 125, 126, 480, 481, 511, 512, 513, 4096, 4097, 65535, 65536, 65537, 1 << 20} {
		for _, op := range []Opcode{BinaryMessage, TextMessage} {
			for _, masked := range []bool{false, true} {
				b.Run(fmt.Sprintf("bytes=%d/type=%s/masked=%t", size, op, masked), func(b *testing.B) {
					payload := benchmarkPayload(size)
					if op == TextMessage {
						payload = bytes.Repeat([]byte{'a'}, size)
					}
					ownershipBenchmarkRead(b, op, payload, masked, 1, nil)
				})
			}
		}
	}
}

func BenchmarkReadMessageOwnershipFallback(b *testing.B) {
	for _, size := range []int{16, 4096, 65536} {
		for _, mode := range []string{"fragmented", "disabled", "enabled", "disabled-pmd", "enabled-pmd-uncompressed"} {
			b.Run(fmt.Sprintf("bytes=%d/mode=%s", size, mode), func(b *testing.B) {
				fragments := 1
				var extensions []Extension
				switch mode {
				case "fragmented":
					fragments = 4
				case "disabled-pmd", "enabled-pmd-uncompressed":
					extensions = []Extension{&perMessageDeflate{enabled: mode == "enabled-pmd-uncompressed"}}
				default:
					extensions = []Extension{&headerTransformExtension{
						enabled: mode == "enabled",
						process: func(*Frame) error { return nil },
					}}
				}
				ownershipBenchmarkRead(b, BinaryMessage, benchmarkPayload(size), true, fragments, extensions)
			})
		}
	}
}

func ownershipBenchmarkRead(b *testing.B, op Opcode, payload []byte, masked bool, fragments int, extensions []Extension) {
	b.Helper()
	wire := benchmarkWire(payload, masked, fragments)
	wire[0] = wire[0]&0xf0 | byte(op)
	r := bytes.NewReader(wire)
	c := NewConn(&benchmarkConn{reader: r, writer: io.Discard}, masked, extensions)
	kind, got, err := c.ReadMessage()
	if err != nil || kind != op || !bytes.Equal(got, payload) {
		b.Fatal(kind, len(got), err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		r.Reset(wire)
		c.rw.Reader.Reset(r)
		kind, got, err = c.ReadMessage()
		if err != nil || kind != op || len(got) != len(payload) {
			b.Fatal(kind, len(got), err)
		}
		ownershipBenchmarkResult = got
	}
}
