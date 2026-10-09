package websocket

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Independently encode peer input at each wire length, including a nonzero mask.
func limitWire(server bool, first byte, payload []byte) []byte {
	b := limitHeader(server, first, len(payload))
	if server {
		key := [4]byte{0x11, 0x22, 0x33, 0x44}
		b = append(b, key[:]...)
		for i, v := range payload {
			b = append(b, v^key[i&3])
		}
		return b
	}
	return append(b, payload...)
}

func limitHeader(server bool, first byte, size int) []byte {
	b := []byte{first, 0}
	switch {
	case size < 126:
		b[1] = byte(size)
	case size < 65536:
		b[1] = 126
		b = binary.BigEndian.AppendUint16(b, uint16(size))
	default:
		b[1] = 127
		b = binary.BigEndian.AppendUint64(b, uint64(size))
	}
	if server {
		b[1] |= 0x80
	}
	return b
}

// Both roles go through their public high-level constructor. The server's
// transport records failure replies; the client uses an actual loopback handshake.
func highLevelReader(t *testing.T, server bool, wire []byte, limits ...int) (*Conn, *memoryConn) {
	t.Helper()
	if server {
		raw := &memoryConn{Reader: bytes.NewReader(wire)}
		w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
		opts := []UpgradeOption{nil}
		for _, n := range limits {
			opts = append(opts, WithUpgradeMaxMessageSize(n))
		}
		c, err := Upgrade(w, upgradeRequest(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		raw.written.Reset()
		t.Cleanup(func() { c.conn.Close() })
		return c, raw
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer raw.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		rw.Write(wire)
		rw.Flush()
	}))
	t.Cleanup(srv.Close)
	opts := []DialOption{nil}
	for _, n := range limits {
		opts = append(opts, WithMaxMessageSize(n))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, _, err := Dial(ctx, "ws"+srv.URL[4:], opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.conn.Close() })
	return c, nil
}

func requireLimitFailure(t *testing.T, c *Conn, raw *memoryConn) {
	t.Helper()
	_, p, err := c.ReadMessage()
	if !errors.Is(err, ErrPayloadTooLarge) || p != nil {
		t.Fatalf("payload %d error %v", len(p), err)
	}
	if _, _, err := c.ReadMessage(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("later read: %v", err)
	}
	if err := c.WriteMessage(TextMessage, []byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("later write: %v", err)
	}
	if raw != nil {
		p := failureClosePayload(t, raw.written.Bytes(), true)
		if binary.BigEndian.Uint16(p) != 1009 {
			t.Fatalf("close code: %x", p)
		}
	}
}

func TestHighLevelDefaultLimit(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, op := range []Opcode{TextMessage, BinaryMessage} {
			for _, fragmented := range []bool{false, true} {
				for _, size := range []int{DefaultMaxMessageSize, DefaultMaxMessageSize + 1} {
					t.Run(fmt.Sprintf("server=%v/%v/fragmented=%v/size=%d", server, op, fragmented, size), func(t *testing.T) {
						payload := bytes.Repeat([]byte{'x'}, size)
						wire := limitWire(server, 0x80|byte(op), payload)
						if fragmented {
							wire = limitWire(server, byte(op), payload[:size/2])
							wire = append(wire, limitWire(server, 0x80, payload[size/2:])...)
						}
						c, raw := highLevelReader(t, server, wire)
						if c.maxBytes != DefaultMaxMessageSize {
							t.Fatalf("default %d", c.maxBytes)
						}
						if size > DefaultMaxMessageSize {
							requireLimitFailure(t, c, raw)
							return
						}
						got, p, err := c.ReadMessage()
						if err != nil || got != op || !bytes.Equal(p, payload) {
							t.Fatalf("read %v %d %v", got, len(p), err)
						}
					})
				}
			}
		}
	}
}

func TestHighLevelDefaultHeaderLimit(t *testing.T) {
	for _, server := range []bool{false, true} {
		t.Run(strconv.FormatBool(server), func(t *testing.T) {
			// No body or masking key exists: the declared size alone must reject.
			c, raw := highLevelReader(t, server, limitHeader(server, 0x82, DefaultMaxMessageSize+1))
			requireLimitFailure(t, c, raw)
		})
	}
	// Also instrument the underlying reader to prove no attempt follows the header.
	raw := &headerOnlyConn{memoryConn: memoryConn{Reader: bytes.NewReader(limitHeader(true, 0x82, DefaultMaxMessageSize+1))}}
	w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
	c, err := Upgrade(w, upgradeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); !errors.Is(err, ErrPayloadTooLarge) || raw.bodyReads != 0 {
		t.Fatalf("error %v body reads %d", err, raw.bodyReads)
	}
}

func TestHighLevelDefaultBudgetSemantics(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, overflow := range []bool{false, true} {
			t.Run(fmt.Sprintf("server=%v/overflow=%v", server, overflow), func(t *testing.T) {
				// U+00E9 consumes two bytes. Controls neither consume nor reset the budget.
				payload := bytes.Repeat([]byte("é"), DefaultMaxMessageSize/2)
				wire := limitWire(server, 0x01, payload)
				wire = append(wire, limitWire(server, 0x89, bytes.Repeat([]byte{'p'}, 125))...)
				wire = append(wire, limitWire(server, 0x8a, bytes.Repeat([]byte{'q'}, 125))...)
				var final []byte
				if overflow {
					final = []byte{'x'}
				}
				wire = append(wire, limitWire(server, 0x80, final)...)
				wire = append(wire, limitWire(server, 0x81, payload)...)
				c, raw := highLevelReader(t, server, wire)
				controls := 0
				c.SetPingHandler(func(string) error { controls++; return nil })
				c.SetPongHandler(func(string) error { controls++; return nil })
				if overflow {
					requireLimitFailure(t, c, raw)
				} else {
					for range 2 {
						op, p, err := c.ReadMessage()
						if op != TextMessage || err != nil || !bytes.Equal(p, payload) {
							t.Fatalf("read %v %d %v", op, len(p), err)
						}
					}
				}
				if controls != 2 {
					t.Fatalf("controls %d", controls)
				}
			})
		}
	}
}

func TestExplicitLimitCompatibility(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			limits     []int
			want, size int
		}{
			{"zero", []int{0}, 0, DefaultMaxMessageSize + 1},
			{"negative", []int{-1}, 0, DefaultMaxMessageSize + 1},
			{"smaller", []int{17}, 17, 17},
			{"larger", []int{DefaultMaxMessageSize + 1}, DefaultMaxMessageSize + 1, DefaultMaxMessageSize + 1},
			{"last zero", []int{17, 0}, 0, DefaultMaxMessageSize + 1},
			{"last negative", []int{17, -1}, 0, DefaultMaxMessageSize + 1},
			{"last positive", []int{0, -1, 17}, 17, 17},
			{"last larger", []int{17, DefaultMaxMessageSize + 1}, DefaultMaxMessageSize + 1, DefaultMaxMessageSize + 1},
		} {
			t.Run(strconv.FormatBool(server)+"/"+tc.name, func(t *testing.T) {
				wire := limitWire(server, 0x82, bytes.Repeat([]byte{'x'}, tc.size))
				if tc.want > 0 {
					wire = append(wire, limitHeader(server, 0x82, tc.want+1)...)
				}
				c, raw := highLevelReader(t, server, wire, tc.limits...)
				if c.maxBytes != tc.want {
					t.Fatalf("limit %d, want %d", c.maxBytes, tc.want)
				}
				_, p, err := c.ReadMessage()
				if err != nil || len(p) != tc.size {
					t.Fatalf("read %d %v", len(p), err)
				}
				if tc.want > 0 {
					requireLimitFailure(t, c, raw)
				}
			})
		}
	}
	for _, tc := range []struct {
		name    string
		options []ConnOption
		want    int
	}{
		{"omitted", nil, 0}, {"zero", []ConnOption{WithMaxBytes(0)}, 0}, {"negative", []ConnOption{WithMaxBytes(-1)}, 0},
		{"positive", []ConnOption{WithMaxBytes(17)}, 17}, {"positive then zero", []ConnOption{WithMaxBytes(17), WithMaxBytes(0)}, 17},
		{"positive then negative", []ConnOption{WithMaxBytes(17), WithMaxBytes(-1)}, 17},
	} {
		t.Run("NewConn/"+tc.name, func(t *testing.T) {
			size := DefaultMaxMessageSize + 1
			if tc.want > 0 {
				size = tc.want
			}
			wire := limitWire(false, 0x82, bytes.Repeat([]byte{'x'}, size))
			if tc.want > 0 {
				wire = append(wire, limitHeader(false, 0x82, size+1)...)
			}
			c := NewConn(&memoryConn{Reader: bytes.NewReader(wire)}, false, nil, tc.options...)
			if c.maxBytes != tc.want {
				t.Fatalf("limit %d want %d", c.maxBytes, tc.want)
			}
			_, p, err := c.ReadMessage()
			if err != nil || len(p) != size {
				t.Fatalf("read %d %v", len(p), err)
			}
			if tc.want > 0 {
				requireLimitFailure(t, c, nil)
			}
		})
	}
}

func TestHighLevelDefaultDoesNotLimitWrites(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, DefaultMaxMessageSize+1)
	c, raw := highLevelReader(t, true, nil)
	if err := c.WriteMessage(BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	_, p, err := frameConn(raw.written.Bytes(), false, 0).ReadMessage()
	if err != nil || !bytes.Equal(p, payload) {
		t.Fatalf("server write %d %v", len(p), err)
	}
	result := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, WithUpgradeMaxMessageSize(0))
		if err != nil {
			result <- err
			return
		}
		defer c.conn.Close()
		_, p, err := c.ReadMessage()
		if err == nil && !bytes.Equal(p, payload) {
			err = errors.New("different payload")
		}
		result <- err
	}))
	defer srv.Close()
	client, _, err := Dial(t.Context(), "ws"+srv.URL[4:])
	if err != nil {
		t.Fatal(err)
	}
	defer client.conn.Close()
	if err := client.WriteMessage(BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestHighLevelDefaultLoopbackServer(t *testing.T) {
	for _, size := range []int{DefaultMaxMessageSize, DefaultMaxMessageSize + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			result := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := Upgrade(w, r)
				if err != nil {
					result <- err
					return
				}
				defer c.conn.Close()
				c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, p, err := c.ReadMessage()
				if err == nil && len(p) != size {
					err = fmt.Errorf("payload length %d", len(p))
				}
				result <- err
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c, _, err := Dial(ctx, "ws"+srv.URL[4:])
			if err != nil {
				t.Fatal(err)
			}
			defer c.conn.Close()
			// Send a header alone for the oversized case; no body is required.
			wire := limitHeader(true, 0x82, size)
			if size == DefaultMaxMessageSize {
				wire = limitWire(true, 0x82, bytes.Repeat([]byte{'x'}, size))
			}
			if _, err := c.conn.Write(wire); err != nil {
				t.Fatal(err)
			}
			err = failureAwait(t, result)
			if size == DefaultMaxMessageSize {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrPayloadTooLarge) {
				t.Fatalf("oversize: %v", err)
			}
		})
	}
}

// limitStallReader signals only after all supplied partial-frame bytes were
// consumed and ReadMessage asks the transport for the deliberately missing byte.
type limitStallReader struct {
	io.Reader
	read    int
	stalled chan struct{}
}

func (r *limitStallReader) Read(p []byte) (int, error) {
	if r.read >= 3 && r.stalled != nil {
		close(r.stalled)
		r.stalled = nil
	}
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func TestHighLevelDefaultCloseInterruptsStalledPeer(t *testing.T) {
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		rw.Write([]byte{0x82, 2, 'x'})
		rw.Flush()
		close(ready)
		// The second byte never arrives. Only caller-owned lifetime control ends I/O.
		io.Copy(io.Discard, raw)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, _, err := Dial(ctx, "ws"+srv.URL[4:])
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	<-ready
	stalled := make(chan struct{})
	c.rw.Reader = bufio.NewReader(&limitStallReader{Reader: c.rw.Reader, stalled: stalled})
	result := make(chan error, 1)
	go func() { _, _, err := c.ReadMessage(); result <- err }()
	select {
	case <-stalled:
	case err := <-result:
		t.Fatalf("read returned before stalling: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("read never reached missing byte")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// The in-flight read retains the detecting transport error. A later read
	// reports io.ErrClosedPipe, as specified by the terminal-error contract.
	if err := failureAwait(t, result); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stalled read: %v", err)
	}
	if _, _, err := c.ReadMessage(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("later read: %v", err)
	}
}
