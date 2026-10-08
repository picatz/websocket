package websocket

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// Keep the RFC's byte-wise algorithm as an independent performance baseline.
func maskScalarReference(key, data []byte) {
	for i := range data {
		data[i] ^= key[i&3]
	}
}

// This portable candidate is intentionally test-only until measurements justify it.
func maskWordCandidate(key, data []byte) {
	if len(data) >= 8 {
		k := uint64(binary.LittleEndian.Uint32(key))
		k |= k << 32
		for len(data) >= 8 {
			binary.LittleEndian.PutUint64(data, binary.LittleEndian.Uint64(data)^k)
			data = data[8:]
		}
	}
	for i := range data {
		data[i] ^= key[i&3]
	}
}

// Explore the standard library's architecture-optimized XORBytes, including
// the cost of expanding a four-byte key into 1 KiB of stack scratch. This is one
// bounded design point, not an exhaustive search for the best SIMD algorithm.
func maskStandardLibraryCandidate(key, data []byte) {
	if len(data) < 1024 {
		maskWordCandidate(key, data)
		return
	}
	var expanded [1024]byte
	k := uint64(binary.LittleEndian.Uint32(key))
	k |= k << 32
	for i := 0; i < len(expanded); i += 8 {
		binary.LittleEndian.PutUint64(expanded[i:], k)
	}
	for len(data) >= len(expanded) {
		subtle.XORBytes(data[:len(expanded)], data[:len(expanded)], expanded[:])
		data = data[len(expanded):]
	}
	maskWordCandidate(key, data)
}

func BenchmarkMask(b *testing.B) {
	for _, size := range []int{0, 1, 7, 8, 16, 125, 126, 1023, 1024, 1025, 4095, 4096, 4097, 65536, 1 << 20} {
		for _, offset := range []int{0, 1} {
			b.Run(fmt.Sprintf("bytes=%d/offset=%d", size, offset), func(b *testing.B) {
				for _, impl := range []struct {
					name string
					mask func([]byte, []byte)
				}{{"production", xor}, {"scalar", maskScalarReference}, {"word", maskWordCandidate}, {"stdlib", maskStandardLibraryCandidate}} {
					b.Run(impl.name, func(b *testing.B) {
						data := make([]byte, size+offset)[offset:]
						key := []byte{0x12, 0x34, 0x56, 0x78}
						b.SetBytes(int64(size))
						b.ReportAllocs()
						for b.Loop() {
							impl.mask(key, data)
						}
					})
				}
			})
		}
	}
}

// benchmarkConn eliminates OS/scheduler cost to isolate framing and allocation.
// It does not model network throughput or application latency.
type benchmarkConn struct {
	reader io.Reader
	writer io.Writer
}

func (c *benchmarkConn) Read(p []byte) (int, error)     { return c.reader.Read(p) }
func (c *benchmarkConn) Write(p []byte) (int, error)    { return c.writer.Write(p) }
func (*benchmarkConn) Close() error                     { return nil }
func (*benchmarkConn) LocalAddr() net.Addr              { return nil }
func (*benchmarkConn) RemoteAddr() net.Addr             { return nil }
func (*benchmarkConn) SetDeadline(time.Time) error      { return nil }
func (*benchmarkConn) SetReadDeadline(time.Time) error  { return nil }
func (*benchmarkConn) SetWriteDeadline(time.Time) error { return nil }

func benchmarkPayload(size int) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(i*31 + 17)
	}
	return p
}

// Construct wire bytes independently of writeFrame and xor. Four fragments use
// a first-fragment length not divisible by four for the benchmarked sizes,
// testing mask-key phase resets.
func benchmarkWire(payload []byte, masked bool, fragments int) []byte {
	var wire []byte
	for i := range fragments {
		n := len(payload) / (fragments - i)
		if i == 0 && fragments > 1 && n > 0 {
			n--
		}
		part := payload[:n]
		payload = payload[n:]
		first := byte(ContinuationFrame)
		if i == 0 {
			first = byte(BinaryMessage)
		}
		if i == fragments-1 {
			first |= 0x80
		}
		var header [14]byte
		header[0] = first
		h := 2
		switch {
		case n <= 125:
			header[1] = byte(n)
		case n <= 65535:
			header[1] = 126
			binary.BigEndian.PutUint16(header[2:], uint16(n))
			h += 2
		default:
			header[1] = 127
			binary.BigEndian.PutUint64(header[2:], uint64(n))
			h += 8
		}
		key := [4]byte{0x21, byte(i + 1), 0x43, 0x65}
		if masked {
			header[1] |= 0x80
			copy(header[h:], key[:])
			h += 4
		}
		wire = append(wire, header[:h]...)
		start := len(wire)
		wire = append(wire, part...)
		if masked {
			maskScalarReference(key[:], wire[start:])
		}
	}
	return wire
}

func BenchmarkReadMessage(b *testing.B) {
	for _, size := range []int{16, 125, 126, 4096, 65536, 1 << 20} {
		for _, masked := range []bool{false, true} {
			for _, fragments := range []int{1, 4} {
				b.Run(fmt.Sprintf("bytes=%d/masked=%t/fragments=%d", size, masked, fragments), func(b *testing.B) {
					payload := benchmarkPayload(size)
					wire := benchmarkWire(payload, masked, fragments)
					r := bytes.NewReader(wire)
					c := NewConn(&benchmarkConn{reader: r, writer: io.Discard}, masked, nil)
					kind, got, err := c.ReadMessage()
					if err != nil || kind != BinaryMessage || !bytes.Equal(got, payload) {
						b.Fatalf("invalid fixture: kind=%v bytes=%d err=%v", kind, len(got), err)
					}
					b.SetBytes(int64(size))
					b.ReportAllocs()
					for b.Loop() {
						r.Reset(wire)
						c.rw.Reader.Reset(r)
						kind, got, err = c.ReadMessage()
						if err != nil || kind != BinaryMessage || len(got) != size {
							b.Fatal(kind, len(got), err)
						}
					}
				})
			}
		}
	}
}

func BenchmarkWriteMessage(b *testing.B) {
	for _, size := range []int{16, 125, 126, 4096, 65536, 1 << 20} {
		for _, server := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/server=%t", size, server), func(b *testing.B) {
				payload := benchmarkPayload(size)
				c := NewConn(&benchmarkConn{writer: io.Discard}, server, nil)
				// Exclude one-time crypto/rand initialization from steady-state results.
				if err := c.WriteMessage(BinaryMessage, payload); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for b.Loop() {
					if err := c.WriteMessage(BinaryMessage, payload); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// A reusable transport includes both endpoints' work. No HTTP handshake, TLS,
// compression, or competing streams are measured. net.Pipe measures synchronous
// in-memory handoff; TCP also includes loopback sockets and the kernel scheduler.
func BenchmarkTransport(b *testing.B) {
	for _, transport := range []string{"pipe", "tcp"} {
		for _, size := range []int{16, 4096, 65536} {
			for _, server := range []bool{false, true} {
				b.Run(fmt.Sprintf("%s/bytes=%d/server=%t", transport, size, server), func(b *testing.B) {
					var sender, receiver net.Conn
					if transport == "pipe" {
						sender, receiver = net.Pipe()
					} else {
						listener, err := net.Listen("tcp", "127.0.0.1:0")
						if err != nil {
							b.Fatal(err)
						}
						defer listener.Close()
						sender, err = net.DialTimeout("tcp", listener.Addr().String(), 5*time.Second)
						if err != nil {
							b.Fatal(err)
						}
						receiver, err = listener.Accept()
						if err != nil {
							sender.Close()
							b.Fatal(err)
						}
					}
					defer sender.Close()
					defer receiver.Close()
					deadline := time.Now().Add(2 * time.Minute)
					if err := sender.SetDeadline(deadline); err != nil {
						b.Fatal(err)
					}
					if err := receiver.SetDeadline(deadline); err != nil {
						b.Fatal(err)
					}
					writer, reader := NewConn(sender, server, nil), NewConn(receiver, !server, nil)
					payload := benchmarkPayload(size)
					done := make(chan error, 1)
					finished := make(chan struct{})
					defer func() {
						sender.Close()
						receiver.Close()
						<-finished
					}()
					b.SetBytes(int64(size))
					b.ReportAllocs()
					b.ResetTimer()
					go func() {
						defer close(finished)
						for i := 0; i < b.N; i++ {
							if err := writer.WriteMessage(BinaryMessage, payload); err != nil {
								sender.Close()
								done <- err
								return
							}
						}
						done <- nil
					}()
					for i := 0; i < b.N; i++ {
						kind, got, err := reader.ReadMessage()
						if err != nil || kind != BinaryMessage || len(got) != size {
							b.Fatal(kind, len(got), err)
						}
					}
					if err := <-done; err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
				})
			}
		}
	}
}

func TestBenchmarkWire(t *testing.T) {
	for _, size := range []int{0, 1, 16, 125, 126, 4096, 65536} {
		for _, masked := range []bool{false, true} {
			for _, fragments := range []int{1, 4} {
				payload := benchmarkPayload(size)
				wire := benchmarkWire(payload, masked, fragments)
				r := bytes.NewReader(wire)
				c := NewConn(&benchmarkConn{reader: r, writer: io.Discard}, masked, nil)
				kind, got, err := c.ReadMessage()
				if err != nil || kind != BinaryMessage || !bytes.Equal(got, payload) {
					t.Fatalf("%d/%t/%d: %v, %v", size, masked, fragments, kind, err)
				}
				if c.rw.Reader.Buffered() != 0 || r.Len() != 0 {
					t.Fatal("unconsumed wire data")
				}
			}
		}
	}
}

// Reuse a prebuilt frame to isolate frame parsing from message assembly.
func BenchmarkReadFrame(b *testing.B) {
	for _, size := range []int{16, 4096, 65536, 1 << 20} {
		for _, masked := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/masked=%t", size, masked), func(b *testing.B) {
				wire := benchmarkWire(benchmarkPayload(size), masked, 1)
				r := bytes.NewReader(wire)
				c := &Conn{rw: bufio.NewReadWriter(bufio.NewReader(r), bufio.NewWriter(io.Discard)), isServer: masked}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for b.Loop() {
					r.Reset(wire)
					c.rw.Reader.Reset(r)
					frame, err := c.readFrame()
					if err != nil || len(frame.Payload) != size {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Check writer output with independent header parsing and scalar unmasking.
func TestBenchmarkWriteWire(t *testing.T) {
	for _, size := range []int{0, 1, 16, 125, 126, 4096, 65535, 65536} {
		for _, server := range []bool{false, true} {
			payload := benchmarkPayload(size)
			original := bytes.Clone(payload)
			var wire bytes.Buffer
			c := NewConn(&benchmarkConn{writer: &wire}, server, nil)
			if err := c.WriteMessage(BinaryMessage, payload); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(payload, original) {
				t.Fatal("writer mutated caller's payload")
			}
			data := wire.Bytes()
			if len(data) < 2 || data[0] != 0x82 || (data[1]&0x80 != 0) == server {
				t.Fatal("invalid frame header or masking direction")
			}
			r := bytes.NewReader(data[2:])
			n := uint64(data[1] & 0x7f)
			switch n {
			case 126:
				var extended uint16
				if err := binary.Read(r, binary.BigEndian, &extended); err != nil {
					t.Fatal(err)
				}
				n = uint64(extended)
				if n < 126 {
					t.Fatal("non-minimal length")
				}
			case 127:
				if err := binary.Read(r, binary.BigEndian, &n); err != nil {
					t.Fatal(err)
				}
				if n < 65536 {
					t.Fatal("non-minimal length")
				}
			}
			if n != uint64(size) {
				t.Fatalf("length=%d, want %d", n, size)
			}
			var key [4]byte
			if !server {
				if _, err := io.ReadFull(r, key[:]); err != nil {
					t.Fatal(err)
				}
			}
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if !server {
				maskScalarReference(key[:], got)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("size=%d server=%t: incorrect wire payload", size, server)
			}
		}
	}
}
