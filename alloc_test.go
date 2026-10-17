package websocket

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		for _, op := range []MessageType{BinaryMessage, TextMessage} {
			for _, masked := range []bool{false, true} {
				t.Run(fmt.Sprintf("bytes=%d/type=%s/masked=%t", size, op, masked), func(t *testing.T) {
					payload := bytes.Repeat([]byte{'a'}, size)
					wire := benchmarkWire(payload, masked, 1)
					wire[0] = wire[0]&0xf0 | byte(op)
					r := bytes.NewReader(wire)
					c := newTestConn(&benchmarkConn{reader: r, writer: io.Discard}, ConnConfig{Server: masked})
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
						c.br.Reset(r)
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
	// Without extensions, fragments are read straight onto the end of the
	// message. Storage doubles with what has arrived, so many small
	// fragments are not copied once per frame.
	for _, tc := range []struct{ size, fragments, max int }{
		{64, 4, 1},       // Everything is buffered: one allocation.
		{65536, 4, 5},    // As many as one 64 KiB frame.
		{1000, 1000, 1},  // One byte per frame, all buffered.
		{8192, 8192, 4},  // More than the read buffer holds: doubling.
		{1 << 20, 64, 9}, // As many as one 1 MiB frame.
	} {
		for _, op := range []MessageType{BinaryMessage, TextMessage} {
			t.Run(fmt.Sprintf("bytes=%d/fragments=%d/type=%s", tc.size, tc.fragments, op), func(t *testing.T) {
				payload := bytes.Repeat([]byte{'a'}, tc.size)
				wire := benchmarkWire(payload, true, tc.fragments)
				wire[0] = wire[0]&0xf0 | byte(op)
				r := bytes.NewReader(wire)
				c := newTestConn(&benchmarkConn{reader: r, writer: io.Discard}, ConnConfig{Server: true})
				got := testing.AllocsPerRun(20, func() {
					r.Reset(wire)
					c.br.Reset(r)
					if kind, p, err := c.ReadMessage(); err != nil || kind != op || !bytes.Equal(p, payload) {
						t.Fatal(kind, len(p), err)
					}
				})
				if got > float64(tc.max) {
					t.Fatalf("allocations = %v, want at most %d", got, tc.max)
				}
			})
		}
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
				c := newTestConn(&benchmarkConn{writer: io.Discard}, ConnConfig{Server: server})
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
					if err := c.writeControlFrame(opPing, payload); err != nil {
						t.Fatal(err)
					}
				}); got != 0 {
					t.Fatalf("writeControlFrame allocations = %v, want 0", got)
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
		got, err := r.readAll(nil, 0)
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
			got, err := r.readAll(nil, 0)
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

func TestPooledWritesRoundTrip(t *testing.T) {
	// Every frame is flushed, so connections borrow a write buffer per frame
	// rather than holding one while idle. Both ends of a real handshake must
	// exchange messages larger than one buffer through the borrowed buffers.
	server := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		server <- c
	}))
	defer srv.Close()
	client, _, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	s := <-server
	defer s.Close()
	payload := bytes.Repeat([]byte("abc"), 5000) // Larger than one write buffer.
	if err := client.WriteMessage(BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	if _, got, err := s.ReadMessage(); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("server read %d bytes, err=%v", len(got), err)
	}
	if err := s.WriteMessage(TextMessage, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, got, err := client.ReadMessage(); err != nil || string(got) != "ok" {
		t.Fatalf("client read %q, err=%v", got, err)
	}
}

func TestReleasedWriterDropsFailedFrame(t *testing.T) {
	// A frame that fails mid-write leaves bytes in the borrowed buffer. They
	// must not reach the next connection that borrows it.
	w := writerPool.Get().(*bufio.Writer)
	w.Reset(io.Discard)
	w.WriteString("stale")
	releaseWriter(&writerPool, w)
	if w.Buffered() != 0 {
		t.Fatalf("released writer keeps %d bytes", w.Buffered())
	}
}

func TestFragmentedReadStaysWithinLimit(t *testing.T) {
	// Storage for a fragmented message grows ahead of later fragments, but a
	// message limit bounds what is allocated as well as what is accepted.
	const limit = 10000
	for _, fragments := range []int{2, 7, 100, limit} {
		payload := bytes.Repeat([]byte{'a'}, limit)
		wire := benchmarkWire(payload, true, fragments)
		c := newTestConn(&benchmarkConn{reader: bytes.NewReader(wire), writer: io.Discard}, ConnConfig{Server: true, MaxMessageSize: limit})
		_, got, err := c.ReadMessage()
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("fragments=%d: read %d bytes, err=%v", fragments, len(got), err)
		}
		if cap(got) > limit {
			t.Fatalf("fragments=%d: capacity %d exceeds the %d byte limit", fragments, cap(got), limit)
		}
	}
}

// writeCounter counts the writes that reach it.
type writeCounter struct {
	bytes.Buffer
	writes int
}

func (w *writeCounter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func TestLargeMaskedWrites(t *testing.T) {
	// A client message too large for the default write buffer is masked
	// through a larger pooled one: few transport writes, the caller's payload
	// untouched, and a server reads back exactly what was sent.
	for _, size := range []int{writeBufferSize - 14, writeBufferSize, writeBufferSize + 1, maskedWriteBufferSize - 1, maskedWriteBufferSize + 1, 100_003} {
		payload := benchmarkPayload(size)
		orig := bytes.Clone(payload)
		var wire writeCounter
		client := newTestConn(&benchmarkConn{writer: &wire}, ConnConfig{})
		if err := client.WriteMessage(BinaryMessage, payload); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, orig) {
			t.Fatalf("size=%d: caller payload modified", size)
		}
		if limit := size/maskedWriteBufferSize + 2; wire.writes > limit {
			t.Errorf("size=%d: %d transport writes, want at most %d", size, wire.writes, limit)
		}
		server := newTestConn(&benchmarkConn{reader: &wire.Buffer, writer: io.Discard}, ConnConfig{Server: true})
		if _, got, err := server.ReadMessage(); err != nil || !bytes.Equal(got, orig) {
			t.Fatalf("size=%d: err=%v equal=%t", size, err, bytes.Equal(got, orig))
		}
	}
}
