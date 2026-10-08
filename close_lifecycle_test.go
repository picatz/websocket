package websocket

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestCloseControlPreventsLaterData(t *testing.T) {
	for _, server := range []bool{false, true} {
		raw := &memoryConn{Reader: bytes.NewReader(nil)}
		c := NewConn(raw, server, nil)
		if err := c.WriteControlFrame(CloseMessage, nil); err != nil {
			t.Fatal(err)
		}
		before := bytes.Clone(raw.written.Bytes())
		for _, opcode := range []Opcode{TextMessage, BinaryMessage} {
			if err := c.WriteMessage(opcode, []byte("late")); !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("server=%v opcode=%v: error %v, want closed pipe", server, opcode, err)
			}
		}
		if !bytes.Equal(before, raw.written.Bytes()) {
			t.Errorf("server=%v: sent data after close: %x", server, raw.written.Bytes())
		}
	}
}

func TestCloseControlCompletesHandshakeOnce(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, peerClose := range []bool{false, true} {
			peer := frameConn(nil, !server, 0)
			if err := peer.WriteControlFrame(CloseMessage, []byte{3, 232}); err != nil {
				t.Fatal(err)
			}
			raw := &memoryConn{Reader: bytes.NewReader(peer.conn.(*memoryConn).written.Bytes())}
			c := NewConn(raw, server, nil)
			if err := c.WriteControlFrame(CloseMessage, nil); err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(raw.written.Bytes())
			if err := c.WriteControlFrame(CloseMessage, nil); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("duplicate close: %v", err)
			}
			if peerClose {
				if _, data, err := c.ReadMessage(); !errors.Is(err, io.EOF) || data != nil {
					t.Fatalf("peer close: %x %v", data, err)
				}
				if _, _, err := c.ReadMessage(); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("read after close: %v", err)
				}
				if err := c.Close(); !errors.Is(err, ErrAlreadyClosed) {
					t.Fatal(err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, raw.written.Bytes()) {
				t.Fatalf("duplicate wire close: %x", raw.written.Bytes())
			}
			if err := c.Close(); !errors.Is(err, ErrAlreadyClosed) {
				t.Fatal(err)
			}
		}
	}
}

func TestCloseControlKeepsReadsAndPongAvailable(t *testing.T) {
	// A split UTF-8 codepoint and ping may already be in flight when we close.
	c := frameConn([]byte{0x01, 1, 0xc2, 0x89, 1, 'p', 0x80, 1, 0xa2, 0x88, 0}, false, 2)
	if err := c.WriteControlFrame(CloseMessage, nil); err != nil {
		t.Fatal(err)
	}
	opcode, data, err := c.ReadMessage()
	if err != nil || opcode != TextMessage || string(data) != "¢" {
		t.Fatalf("read in flight: %v %x %v", opcode, data, err)
	}
	if _, _, err := c.ReadMessage(); err != io.EOF {
		t.Fatal(err)
	}
	wire := frameConn(c.conn.(*memoryConn).written.Bytes(), true, 0)
	closeFrame, err := wire.readFrame()
	if err != nil || closeFrame.Opcode != CloseMessage {
		t.Fatalf("close: %v %v", closeFrame, err)
	}
	pong, err := wire.readFrame()
	if err != nil || pong.Opcode != PongMessage || string(pong.Payload) != "p" {
		t.Fatalf("pong: %v %v", pong, err)
	}
	if _, err := wire.readFrame(); err != io.EOF {
		t.Fatalf("extra frame: %v", err)
	}
}

func TestRejectedCloseControlDoesNotCloseWriteSide(t *testing.T) {
	c := frameConn(nil, true, 0)
	for _, payload := range [][]byte{{1}, {3, 237}, append([]byte{3, 232}, bytes.Repeat([]byte{'a'}, 124)...)} {
		if err := c.WriteControlFrame(CloseMessage, payload); err == nil {
			t.Fatal("accepted invalid close")
		}
	}
	if err := c.WriteMessage(TextMessage, []byte("ok")); err != nil {
		t.Fatal(err)
	}
}

type failingCloseConn struct {
	*memoryConn
	partial bool
	calls   int
}

func (c *failingCloseConn) Write(p []byte) (int, error) {
	c.calls++
	if c.partial {
		c.written.Write(p[:1])
		return 1, io.ErrClosedPipe
	}
	return 0, io.ErrClosedPipe
}
func TestFailedCloseControlPreservesTransportError(t *testing.T) {
	for _, partial := range []bool{false, true} {
		raw := &failingCloseConn{memoryConn: &memoryConn{Reader: bytes.NewReader(nil)}, partial: partial}
		c := NewConn(raw, true, nil)
		for _, write := range []func() error{
			func() error { return c.WriteControlFrame(CloseMessage, []byte{3, 232}) },
			func() error { return c.WriteMessage(TextMessage, []byte("late")) },
			func() error { return c.WriteControlFrame(CloseMessage, nil) },
		} {
			if err := write(); !errors.Is(err, ErrWriteFailed) || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("partial=%v: %v", partial, err)
			}
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if raw.calls != 1 {
			t.Fatalf("retried failed transport: %d", raw.calls)
		}
	}
}

func TestConcurrentCloseControlOrdering(t *testing.T) {
	for range 100 {
		raw := &memoryConn{Reader: bytes.NewReader(nil)}
		c := NewConn(raw, true, nil)
		start := make(chan struct{})
		results := make(chan error, 3)
		go func() { <-start; results <- c.WriteMessage(BinaryMessage, []byte("data")) }()
		for range 2 {
			go func() { <-start; results <- c.WriteControlFrame(CloseMessage, nil) }()
		}
		close(start)
		for range 3 {
			if err := <-results; err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
		}
		wire := frameConn(raw.written.Bytes(), false, 0)
		seenClose := false
		for {
			frame, err := wire.readFrame()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if seenClose {
				t.Fatal("frame after close")
			}
			seenClose = frame.Opcode == CloseMessage
		}
		if !seenClose {
			t.Fatal("no close frame")
		}
	}
}

func TestPeerAndLocalCloseRace(t *testing.T) {
	for range 100 {
		c := frameConn([]byte{0x88, 0}, false, 0)
		start := make(chan struct{})
		done := make(chan struct{}, 3)
		for _, fn := range []func(){func() { c.ReadMessage() }, func() { c.WriteControlFrame(CloseMessage, nil) }, func() { c.Close() }} {
			go func() { <-start; fn(); done <- struct{}{} }()
		}
		close(start)
		for range 3 {
			<-done
		}
		wire := frameConn(c.conn.(*memoryConn).written.Bytes(), true, 0)
		count := 0
		for {
			frame, err := wire.readFrame()
			if err == io.EOF {
				break
			}
			if err != nil || frame.Opcode != CloseMessage {
				t.Fatalf("unexpected frame %v, %v", frame, err)
			}
			count++
		}
		if count > 1 {
			t.Fatalf("duplicate close: %d", count)
		}
		if !c.closed.Load() {
			t.Fatal("transport not closed")
		}
	}
}
