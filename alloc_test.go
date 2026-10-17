package websocket

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"testing"
)

// The hot paths allocate only what a caller keeps: ReadMessage allocates the
// returned payload and WriteMessage allocates nothing, with or without
// masking. These pin the counts so a regression shows up as a test failure,
// not only as a benchmark drift.

func TestReadMessageAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful with the race detector")
	}
	for _, size := range []int{0, 1, 16, 125, 126, 4096, 65536} {
		for _, op := range []Opcode{BinaryMessage, TextMessage} {
			for _, masked := range []bool{false, true} {
				t.Run(fmt.Sprintf("bytes=%d/type=%s/masked=%t", size, op, masked), func(t *testing.T) {
					payload := bytes.Repeat([]byte{'a'}, size)
					wire := benchmarkWire(payload, masked, 1)
					wire[0] = wire[0]&0xf0 | byte(op)
					r := bytes.NewReader(wire)
					c := NewConn(&benchmarkConn{reader: r, writer: io.Discard}, masked, nil)
					want := 1.0
					if size == 0 {
						want = 0
					} else if size > minPayloadAlloc {
						// Storage doubles from minPayloadAlloc as bytes arrive,
						// unless the whole payload is already buffered.
						want = -1
					}
					got := testing.AllocsPerRun(100, func() {
						r.Reset(wire)
						c.rw.Reader.Reset(r)
						kind, p, err := c.ReadMessage()
						if err != nil || kind != op || len(p) != size {
							t.Fatal(kind, len(p), err)
						}
					})
					if want >= 0 && got != want {
						t.Fatalf("allocations = %v, want %v", got, want)
					}
					if want < 0 && got > 6 {
						t.Fatalf("allocations = %v, want at most 6", got)
					}
				})
			}
		}
	}
}

func TestReadMessageFragmentedAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful with the race detector")
	}
	payload := bytes.Repeat([]byte{'a'}, 64)
	wire := benchmarkWire(payload, true, 4)
	r := bytes.NewReader(wire)
	c := NewConn(&benchmarkConn{reader: r, writer: io.Discard}, true, nil)
	// One payload per frame plus the growing message.
	got := testing.AllocsPerRun(100, func() {
		r.Reset(wire)
		c.rw.Reader.Reset(r)
		if _, p, err := c.ReadMessage(); err != nil || len(p) != len(payload) {
			t.Fatal(len(p), err)
		}
	})
	if got > 8 {
		t.Fatalf("allocations = %v, want at most 8", got)
	}
}

func TestWriteAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful with the race detector")
	}
	for _, size := range []int{0, 16, 125, 126, 4096, 65536} {
		for _, server := range []bool{false, true} {
			t.Run(fmt.Sprintf("bytes=%d/server=%t", size, server), func(t *testing.T) {
				payload := benchmarkPayload(size)
				c := NewConn(&benchmarkConn{writer: io.Discard}, server, nil)
				if got := testing.AllocsPerRun(100, func() {
					if err := c.WriteMessage(BinaryMessage, payload); err != nil {
						t.Fatal(err)
					}
				}); got != 0 {
					t.Fatalf("WriteMessage allocations = %v, want 0", got)
				}
				if size > 125 {
					return
				}
				if got := testing.AllocsPerRun(100, func() {
					if err := c.WriteControlFrame(PingMessage, payload); err != nil {
						t.Fatal(err)
					}
				}); got != 0 {
					t.Fatalf("WriteControlFrame allocations = %v, want 0", got)
				}
			})
		}
	}
}

func TestPayloadReaderGrowsWithReceivedBytes(t *testing.T) {
	// A peer may declare a frame far larger than it sends. Storage must follow
	// the bytes that arrive, not the declaration.
	const declared = 1 << 20
	for _, sent := range []int{0, 1, 10, minPayloadAlloc, minPayloadAlloc + 1, 3 * minPayloadAlloc} {
		data := benchmarkPayload(sent)
		r := payloadReader{r: bufio.NewReaderSize(bytes.NewReader(data), 16), remaining: declared}
		got, err := r.readAll()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("sent=%d: got %d bytes, err=%v", sent, len(got), err)
		}
		if limit := max(minPayloadAlloc, 2*sent); cap(got) > limit {
			t.Fatalf("sent=%d: capacity %d exceeds %d", sent, cap(got), limit)
		}
	}
}

func TestPayloadReaderExactCapacity(t *testing.T) {
	// ReadMessage keeps a single frame's payload as the message only when it
	// has no spare capacity.
	key := [4]byte{0x9d, 0x62, 0xab, 0xc4}
	for _, size := range []int{1, 15, 16, minPayloadAlloc - 1, minPayloadAlloc, minPayloadAlloc + 1, 5*minPayloadAlloc + 3, 1 << 20} {
		for _, masked := range []bool{false, true} {
			want := benchmarkPayload(size)
			wire := bytes.Clone(want)
			if masked {
				maskScalarReference(key[:], wire)
			}
			source := &streamChunkReader{data: wire, sizes: []int{1, 7, 4093, 65536}, tail: io.EOF}
			r := payloadReader{r: bufio.NewReader(source), remaining: int64(size), key: key, masked: masked}
			got, err := r.readAll()
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("size=%d masked=%t: err=%v equal=%t", size, masked, err, bytes.Equal(got, want))
			}
			if cap(got) != len(got) {
				t.Fatalf("size=%d masked=%t: cap %d, len %d", size, masked, cap(got), len(got))
			}
		}
	}
}

func TestWriteMaskedEveryChunkBoundary(t *testing.T) {
	key := [4]byte{0x11, 0x22, 0x33, 0x44}
	for _, size := range []int{16, 17, 18, 19, 20, 64} {
		for _, buffered := range []int{0, 1, 2, 3, 15} {
			for n := range 70 {
				payload := benchmarkPayload(n)
				orig := bytes.Clone(payload)
				var out bytes.Buffer
				w := bufio.NewWriterSize(&out, size)
				w.Write(make([]byte, buffered))
				if err := writeMasked(w, key, payload); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				want := bytes.Clone(orig)
				maskScalarReference(key[:], want)
				if got := out.Bytes()[buffered:]; !bytes.Equal(got, want) {
					t.Fatalf("size=%d buffered=%d n=%d: got %x, want %x", size, buffered, n, got, want)
				}
				if !bytes.Equal(payload, orig) {
					t.Fatalf("size=%d buffered=%d n=%d: caller payload modified", size, buffered, n)
				}
			}
		}
	}
}
