package websocket

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"
	"time"
)

// clientWire builds unmasked server-to-client frames.
func clientWire(frames ...[]byte) []byte { return bytes.Join(frames, nil) }

func TestNextReaderStreamsFragmentsAndControls(t *testing.T) {
	wire := clientWire(
		failureWire(false, 0x02, []byte("hello, ")),
		failureWire(false, 0x89, []byte("ping")), // answered automatically
		failureWire(false, 0x00, []byte("stream")),
		failureWire(false, 0x80, []byte("ing world")),
		failureWire(false, 0x81, []byte("next")),
	)
	raw := &memoryConn{Reader: bytes.NewReader(wire)}
	c := newTestConn(raw, ConnConfig{})
	typ, r, err := c.NextReader()
	if err != nil || typ != BinaryMessage {
		t.Fatalf("NextReader = %v, %v", typ, err)
	}
	got, err := io.ReadAll(iotest.OneByteReader(r))
	if err != nil || string(got) != "hello, streaming world" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("Read after EOF = %d, %v", n, err)
	}
	if pong, err := frameConn(raw.written.Bytes(), true, 0).readFrame(); err != nil || pong.Opcode != PongMessage || string(pong.Payload) != "ping" {
		t.Fatalf("automatic pong = %+v, %v", pong, err)
	}
	if typ, data, err := c.ReadMessage(); err != nil || typ != TextMessage || string(data) != "next" {
		t.Fatalf("next message = %v %q %v", typ, data, err)
	}
}

func TestNextReaderTextValidation(t *testing.T) {
	euro := []byte("€")
	tests := []struct {
		name  string
		wire  []byte
		valid bool
	}{
		{"rune across frames", clientWire(failureWire(false, 0x01, euro[:1]), failureWire(false, 0x80, euro[1:])), true},
		{"invalid in second frame", clientWire(failureWire(false, 0x01, []byte("ok")), failureWire(false, 0x80, []byte{0xff})), false},
		{"incomplete at end", clientWire(failureWire(false, 0x01, []byte("a")), failureWire(false, 0x80, euro[:2])), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(tt.wire)}}
			c := newTestConn(raw, ConnConfig{})
			_, r, err := c.NextReader()
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(r)
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("ReadAll = %v, want ErrInvalidFrame", err)
			}
			if got := failureClosePayload(t, raw.written.Bytes(), false); !bytes.Equal(got, []byte{0x03, 0xef}) {
				t.Fatalf("close payload = %x, want 1007", got)
			}
		})
	}
}

// The size limit covers the whole message; an oversized continuation fails
// before any of its payload is returned.
func TestNextReaderMessageLimit(t *testing.T) {
	wire := clientWire(failureWire(false, 0x02, bytes.Repeat([]byte{'a'}, 6)), failureWire(false, 0x80, bytes.Repeat([]byte{'b'}, 6)))
	raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(wire)}}
	c := newTestConn(raw, ConnConfig{Server: false, MaxMessageSize: 10})
	_, r, err := c.NextReader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if !errors.Is(err, ErrPayloadTooLarge) || string(got) != "aaaaaa" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
	if got := failureClosePayload(t, raw.written.Bytes(), false); !bytes.Equal(got, []byte{0x03, 0xf1}) {
		t.Fatalf("close payload = %x, want 1009", got)
	}
	if _, _, err := c.NextReader(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("NextReader after failure = %v", err)
	}
}

func TestNextReaderPeerClose(t *testing.T) {
	closeFrame := failureWire(false, 0x88, append([]byte{0x03, 0xe9}, "bye"...))
	for name, wire := range map[string][]byte{
		"before message": closeFrame,
		"inside message": clientWire(failureWire(false, 0x02, []byte("part")), closeFrame),
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestConn(&memoryConn{Reader: bytes.NewReader(wire)}, ConnConfig{})
			_, r, err := c.NextReader()
			if err == nil {
				_, err = io.ReadAll(r)
			}
			if ce, ok := errors.AsType[*CloseError](err); !ok || ce.Code != StatusGoingAway || ce.Reason != "bye" {
				t.Fatalf("error = %v, want the peer's CloseError", err)
			}
		})
	}
}

func TestNextReaderDiscardsUnreadRest(t *testing.T) {
	wire := clientWire(
		failureWire(false, 0x02, []byte("first ")),
		failureWire(false, 0x80, []byte("message")),
		failureWire(false, 0x81, []byte("second")),
	)
	c := newTestConn(&memoryConn{Reader: bytes.NewReader(wire)}, ConnConfig{})
	_, first, err := c.NextReader()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Read(make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.ReadMessage(); err != nil || string(data) != "second" {
		t.Fatalf("ReadMessage = %q, %v", data, err)
	}
	if _, err := first.Read(make([]byte, 3)); !errors.Is(err, errReaderSuperseded) {
		t.Fatalf("superseded Read = %v", err)
	}
}

func TestNextReaderCompressed(t *testing.T) {
	payload := bytes.Repeat([]byte("compressed stream "), 30000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&Upgrader{Compression: &Compression{}}).Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if err := conn.WriteMessage(TextMessage, payload); err != nil {
			t.Error(err)
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := (&Dialer{Compression: &Compression{}}).Dial(ctx, "ws"+srv.URL[4:])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	typ, r, err := conn.NextReader()
	if err != nil || typ != TextMessage {
		t.Fatalf("NextReader = %v, %v", typ, err)
	}
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("ReadAll = %d bytes, %v", len(got), err)
	}
}

// NextReader must accept, reject and answer exactly what ReadMessage does.
func FuzzNextReaderMatchesReadMessage(f *testing.F) {
	for _, wire := range [][]byte{
		{0x81, 1, 'x'}, {0x01, 1, 'a', 0x80, 1, 'b'}, {0x89, 0, 0x81, 0}, {0x88, 0},
		{0x01, 1, 0xe2, 0x89, 0, 0x80, 2, 0x82, 0xac}, {0x02, 4, 'a', 'b', 'c', 'd', 0x80, 4, 'e', 'f', 'g', 'h'},
		{0x01, 1, 0xff, 0x80, 0}, {0x02, 1, 'a', 0x82, 0}, {0x80, 0}, {0x02, 1, 'a'},
	} {
		f.Add(wire, false)
	}
	f.Add([]byte{0x82, 0x81, 1, 2, 3, 4, 'x' ^ 1}, true)
	f.Fuzz(func(t *testing.T, wire []byte, server bool) {
		const limit = 6
		wantRaw := &memoryConn{Reader: bytes.NewReader(wire)}
		wantType, want, wantErr := newTestConn(wantRaw, ConnConfig{Server: server, MaxMessageSize: limit}).ReadMessage()

		gotRaw := &memoryConn{Reader: bytes.NewReader(wire)}
		gotType, r, gotErr := newTestConn(gotRaw, ConnConfig{Server: server, MaxMessageSize: limit}).NextReader()
		var got []byte
		if gotErr == nil {
			got, gotErr = io.ReadAll(r)
		}
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("ReadMessage error %v, NextReader error %v", wantErr, gotErr)
		}
		if wantErr == nil && (wantType != gotType || !bytes.Equal(want, got)) {
			t.Fatalf("ReadMessage %v %q, NextReader %v %q", wantType, want, gotType, got)
		}
		if CloseStatus(wantErr) != CloseStatus(gotErr) {
			t.Fatalf("peer close: ReadMessage %v, NextReader %v", wantErr, gotErr)
		}
		if !bytes.Equal(wantRaw.written.Bytes(), gotRaw.written.Bytes()) {
			// Compare frame headers and unmasked payloads; masking keys differ.
			if !sameFrames(t, wantRaw.written.Bytes(), gotRaw.written.Bytes(), server) {
				t.Fatalf("replies differ: ReadMessage %x, NextReader %x", wantRaw.written.Bytes(), gotRaw.written.Bytes())
			}
		}
	})
}

func sameFrames(t *testing.T, a, b []byte, server bool) bool {
	t.Helper()
	read := func(wire []byte) []wireFrame {
		c := newTestConn(&memoryConn{Reader: bytes.NewReader(wire)}, ConnConfig{Server: !server})
		var frames []wireFrame
		for {
			f, err := c.readFrame()
			if err != nil {
				return frames
			}
			frames = append(frames, wireFrame{Final: f.Final, Opcode: f.Opcode, Payload: f.Payload})
		}
	}
	fa, fb := read(a), read(b)
	if len(fa) != len(fb) {
		return false
	}
	for i := range fa {
		if fa[i].Final != fb[i].Final || fa[i].Opcode != fb[i].Opcode || !bytes.Equal(fa[i].Payload, fb[i].Payload) {
			return false
		}
	}
	return true
}
