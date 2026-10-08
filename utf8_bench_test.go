package websocket

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

func BenchmarkReadTextMessage(b *testing.B) {
	for _, size := range []int{16, 4096, 65536} {
		for _, multibyte := range []bool{false, true} {
			for _, fragments := range []int{1, 4, 128} {
				if fragments > size {
					continue
				}
				b.Run(fmt.Sprintf("bytes=%d/multibyte=%t/fragments=%d", size, multibyte, fragments), func(b *testing.B) {
					payload := bytes.Repeat([]byte("a"), size)
					if multibyte {
						payload = bytes.Repeat([]byte("𐀀"), size/4)
					}
					wire := benchmarkWire(payload, false, fragments)
					wire[0] = wire[0]&0xf0 | byte(TextMessage)
					reader := bytes.NewReader(wire)
					c := NewConn(&benchmarkConn{reader: reader, writer: io.Discard}, false, nil)
					b.SetBytes(int64(size))
					b.ReportAllocs()
					for b.Loop() {
						reader.Reset(wire)
						c.rw.Reader.Reset(reader)
						op, p, err := c.ReadMessage()
						if err != nil || op != TextMessage || len(p) != len(payload) {
							b.Fatal(op, len(p), err)
						}
					}
				})
			}
		}
	}
}
