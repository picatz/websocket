package websocket

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
	"unicode/utf8"
)

// streamFrames parses everything written to raw as frames from the given role.
func streamFrames(t *testing.T, raw *memoryConn, fromServer bool) []*wireFrame {
	t.Helper()
	wire := newTestConn(&memoryConn{Reader: bytes.NewReader(raw.written.Bytes())}, ConnConfig{Server: !fromServer})
	var frames []*wireFrame
	for {
		f, err := wire.readFrame()
		if err == io.EOF {
			return frames
		}
		if err != nil {
			t.Fatalf("frame %d: %v", len(frames), err)
		}
		frames = append(frames, f)
	}
}

func TestNextWriterFragmentsLargeMessages(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 3*writeFragmentSize/16+7)
	for _, server := range []bool{false, true} {
		raw := &memoryConn{Reader: bytes.NewReader(nil)}
		c := newTestConn(raw, ConnConfig{Server: server})
		w, err := c.NextWriter(BinaryMessage)
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range slices.Chunk(payload, 1000) {
			if n, err := w.Write(chunk); err != nil || n != len(chunk) {
				t.Fatalf("Write = %d, %v", n, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		frames := streamFrames(t, raw, server)
		if len(frames) != 4 {
			t.Fatalf("server=%v: %d frames, want 4", server, len(frames))
		}
		var got []byte
		for i, f := range frames {
			wantOp := opContinuation
			if i == 0 {
				wantOp = opBinary
			}
			if f.Opcode != wantOp || f.Final != (i == len(frames)-1) {
				t.Fatalf("frame %d: opcode %v final %v", i, f.Opcode, f.Final)
			}
			got = append(got, f.Payload...)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("server=%v: payload differs", server)
		}
	}
}

func TestNextWriterEmptyMessage(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := newTestConn(raw, ConnConfig{Server: true})
	w, err := c.NextWriter(TextMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if frames := streamFrames(t, raw, true); len(frames) != 1 || !frames[0].Final || frames[0].Opcode != opText || len(frames[0].Payload) != 0 {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestNextWriterValidatesTextAcrossWrites(t *testing.T) {
	euro := []byte("€") // e2 82 ac
	tests := []struct {
		name   string
		writes [][]byte
		valid  bool
	}{
		{"rune split 1+2", [][]byte{euro[:1], euro[1:]}, true},
		{"rune split 1+1+1", [][]byte{euro[:1], euro[1:2], euro[2:]}, true},
		{"rune split with tail", [][]byte{append([]byte("a"), euro[:2]...), append(euro[2:], "b€"...)}, true},
		{"empty writes", [][]byte{nil, euro[:2], nil, euro[2:]}, true},
		{"invalid byte", [][]byte{[]byte("ok"), {0xff}}, false},
		{"bad continuation", [][]byte{euro[:1], []byte("x")}, false},
		{"surrogate prefix", [][]byte{{0xed}, {0xa0, 0x80}}, false},
		{"incomplete at close", [][]byte{[]byte("a"), euro[:2]}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := &memoryConn{Reader: bytes.NewReader(nil)}
			c := newTestConn(raw, ConnConfig{Server: true})
			w, err := c.NextWriter(TextMessage)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range tt.writes {
				if _, err = w.Write(p); err != nil {
					break
				}
			}
			if err == nil {
				err = w.Close()
			}
			if tt.valid != (err == nil) || (!tt.valid && !errors.Is(err, ErrInvalidFrame)) {
				t.Fatalf("error = %v, valid = %v", err, tt.valid)
			}
			if !tt.valid {
				// Nothing reached the wire, so the connection stays usable.
				if raw.written.Len() != 0 {
					t.Fatalf("sent %x for an invalid message", raw.written.Bytes())
				}
				if err := c.WriteMessage(TextMessage, []byte("next")); err != nil {
					t.Fatalf("WriteMessage after rejected text: %v", err)
				}
			}
		})
	}
}

func TestNextWriterUseAfterClose(t *testing.T) {
	c := newTestConn(&memoryConn{Reader: bytes.NewReader(nil)}, ConnConfig{Server: true})
	w, err := c.NextWriter(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, errWriterClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
	if err := w.Close(); !errors.Is(err, errWriterClosed) {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := c.NextWriter(MessageType(opPing)); !errors.Is(err, errInvalidMessageType) {
		t.Fatalf("NextWriter(Ping) = %v", err)
	}
}

// Data messages wait for the open writer; control frames go between its frames.
func TestNextWriterSerializesDataAndInterleavesControl(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := newTestConn(raw, ConnConfig{Server: true})
	w, err := c.NextWriter(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte{'a'}, writeFragmentSize+1)); err != nil {
		t.Fatal(err)
	}

	queued := make(chan error, 1)
	go func() { queued <- c.WriteMessage(TextMessage, []byte("second")) }()
	select {
	case err := <-queued:
		t.Fatalf("WriteMessage did not wait for the open writer: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := c.writeControlFrame(opPong, []byte("p")); err != nil {
		t.Fatalf("control frame blocked or failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, f := range streamFrames(t, raw, true) {
		got = append(got, f.Opcode.String())
	}
	want := []string{"binary", "pong", "continuation", "text"}
	if !slices.Equal(got, want) {
		t.Fatalf("frame order = %v, want %v", got, want)
	}
}

// An abandoned writer must not block writes once the connection is closed.
func TestNextWriterAbandonedDoesNotBlockAfterClose(t *testing.T) {
	c := newTestConn(&memoryConn{Reader: bytes.NewReader(nil)}, ConnConfig{Server: true})
	w, err := c.NextWriter(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan error, 1)
	go func() { queued <- c.WriteMessage(BinaryMessage, []byte("x")) }()
	time.Sleep(20 * time.Millisecond)
	_ = c.Close()
	select {
	case err := <-queued:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("queued WriteMessage = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WriteMessage stayed blocked behind an abandoned writer")
	}
	if _, err := w.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write after Conn.Close = %v", err)
	}
}

// A close frame sent between fragments ends the message: no data may follow.
func TestNextWriterStopsAfterCloseFrame(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := newTestConn(raw, ConnConfig{Server: true})
	w, _ := c.NextWriter(BinaryMessage)
	if _, err := w.Write(bytes.Repeat([]byte{'a'}, writeFragmentSize+1)); err != nil {
		t.Fatal(err)
	}
	if err := c.writeControlFrame(opClose, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Close after close frame = %v", err)
	}
	if frames := streamFrames(t, raw, true); len(frames) != 2 || frames[1].Opcode != opClose {
		t.Fatalf("frames = %d", len(frames))
	}
}

// Over a real negotiated connection with compression and context takeover,
// streamed messages arrive intact and the history carries between them.
func TestNextWriterCompressedRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("streamed compressible text "), 20000) // ~540 KB
	received := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&Upgrader{Compression: &Compression{}, MaxMessageSize: 1 << 21}).Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for range 2 {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			received <- data
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := (&Dialer{Compression: &Compression{}}).Dial(ctx, "ws"+srv.URL[4:])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for range 2 {
		w, err := conn.NextWriter(TextMessage)
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range slices.Chunk(payload, 4093) {
			if _, err := w.Write(chunk); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 {
		if got := <-received; !bytes.Equal(got, payload) {
			t.Fatalf("message %d: got %d bytes, want %d", i, len(got), len(payload))
		}
	}
}

// Splitting a payload anywhere must not change whether it is valid UTF-8.
func FuzzUTF8StreamSplits(f *testing.F) {
	for _, seed := range []string{"", "a€b", "\xe2\x82", "\xed\xa0\x80", "ünï\xffcode", "𝄞𝄞", "\xf4\x90\x80\x80"} {
		f.Add([]byte(seed), uint8(1), uint8(2))
	}
	f.Fuzz(func(t *testing.T, p []byte, a, b uint8) {
		i, j := min(int(a), len(p)), min(int(a)+int(b), len(p))
		var s utf8Stream
		got := s.check(p[:i]) && s.check(p[i:j]) && s.check(p[j:]) && s.pending == 0
		if want := utf8.Valid(p); got != want {
			t.Fatalf("split %d/%d of %x: streamed valid = %v, utf8.Valid = %v", i, j, p, got, want)
		}
	})
}
