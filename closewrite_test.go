package websocket_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/picatz/websocket"
)

// Frame opcodes the scripted peer sees (RFC 6455 Section 5.2).
const (
	wireText  = 0x1
	wireClose = 0x8
	wirePing  = 0x9
	wirePong  = 0xa
)

// peerFrame is a frame the scripted peer received from the client.
type peerFrame struct {
	opcode  byte // RFC 6455 Section 5.2
	payload []byte
}

// scriptedPeer is the server end of a net.Pipe. It records every frame the
// client sends, and sends raw server frames on request.
type scriptedPeer struct {
	conn   net.Conn
	frames chan peerFrame // closed when the client's side of the pipe ends
}

func newScriptedPeer(t *testing.T) (*websocket.Conn, *scriptedPeer) {
	t.Helper()
	a, b := net.Pipe()
	p := &scriptedPeer{conn: b, frames: make(chan peerFrame, 256)}
	go func() {
		defer close(p.frames)
		for {
			f, err := readClientFrame(b)
			if err != nil {
				return
			}
			p.frames <- f
		}
	}()
	t.Cleanup(func() { b.Close() })
	c, err := websocket.NewConn(a, websocket.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

// readClientFrame reads one masked, unfragmented client frame.
func readClientFrame(r io.Reader) (peerFrame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return peerFrame{}, err
	}
	n := int(h[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return peerFrame{}, err
		}
		n = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		return peerFrame{}, errors.New("frame too large for the scripted peer")
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return peerFrame{}, err
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return peerFrame{}, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return peerFrame{opcode: h[0] & 0x0f, payload: payload}, nil
}

// send writes raw server frames in the background, since net.Pipe blocks
// until the client reads them.
func (p *scriptedPeer) send(frames ...[]byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		var err error
		for _, f := range frames {
			if _, err = p.conn.Write(f); err != nil {
				break
			}
		}
		done <- err
	}()
	return done
}

// next returns the next frame the client sent, failing the test if the client
// sends nothing within a second.
func (p *scriptedPeer) next(t *testing.T) peerFrame {
	t.Helper()
	select {
	case f, ok := <-p.frames:
		if !ok {
			t.Fatal("client closed the connection; want another frame")
		}
		return f
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a frame from the client")
	}
	return peerFrame{}
}

// rest returns every frame the client sends until its side of the pipe ends.
func (p *scriptedPeer) rest(t *testing.T) []peerFrame {
	t.Helper()
	var frames []peerFrame
	timeout := time.After(2 * time.Second)
	for {
		select {
		case f, ok := <-p.frames:
			if !ok {
				return frames
			}
			frames = append(frames, f)
		case <-timeout:
			t.Fatal("client did not close the connection")
		}
	}
}

func textFrame(s string) []byte { return append([]byte{0x81, byte(len(s))}, s...) }

var (
	normalCloseFrame = []byte{0x88, 2, 0x03, 0xe8} // StatusNormalClosure
	pingFrame        = []byte{0x89, 2, 'h', 'i'}
)

func TestCloseWriteInvalidStatusLeavesConnOpen(t *testing.T) {
	c, p := newScriptedPeer(t)
	for _, tc := range []struct {
		code   websocket.StatusCode
		reason string
	}{
		{999, ""},
		{websocket.StatusNoStatusReceived, ""},
		{websocket.StatusNormalClosure, string(make([]byte, 124))},
		{websocket.StatusNormalClosure, "\xff"},
	} {
		if err := c.CloseWrite(tc.code, tc.reason); !errors.Is(err, websocket.ErrInvalidCloseStatus) {
			t.Errorf("CloseWrite(%d, %d bytes): err = %v, want ErrInvalidCloseStatus", tc.code, len(tc.reason), err)
		}
	}
	errc := make(chan error, 1)
	go func() { errc <- c.WriteMessage(context.Background(), websocket.TextMessage, []byte("still open")) }()
	if f := p.next(t); f.opcode != wireText || string(f.payload) != "still open" {
		t.Fatalf("first frame = %#x %q, want the text message", f.opcode, f.payload)
	}
	if err := <-errc; err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
}

func TestCloseWriteDrainsPeerMessages(t *testing.T) {
	c, p := newScriptedPeer(t)
	errc := make(chan error, 1)
	go func() { errc <- c.CloseWrite(websocket.StatusGoingAway, "done") }()
	f := p.next(t)
	if f.opcode != wireClose || binary.BigEndian.Uint16(f.payload) != uint16(websocket.StatusGoingAway) || string(f.payload[2:]) != "done" {
		t.Fatalf("first frame = %#x %q, want Close 1001 done", f.opcode, f.payload)
	}
	if err := <-errc; err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for name, err := range map[string]error{
		"WriteMessage":          c.WriteMessage(context.Background(), websocket.TextMessage, []byte("x")),
		"WriteMessage with ctx": c.WriteMessage(ctx, websocket.BinaryMessage, []byte("x")),
		"Ping":                  c.Ping(ctx),
		"second CloseWrite":     c.CloseWrite(websocket.StatusNormalClosure, ""),
	} {
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s after CloseWrite: err = %v, want a match for net.ErrClosed", name, err)
		}
	}

	sent := p.send(textFrame("one"), pingFrame, textFrame("two"), normalCloseFrame)
	for _, want := range []string{"one", "two"} {
		typ, data, err := c.ReadMessage(context.Background())
		if err != nil || typ != websocket.TextMessage || string(data) != want {
			t.Fatalf("ReadMessage = %v %q %v, want %q", typ, data, err, want)
		}
	}
	_, _, err := c.ReadMessage(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("ReadMessage after the drain: err = %v, want the peer's CloseError", err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("peer send: %v", err)
	}
	// The only frame after the Close is the Pong answering the peer's Ping,
	// and the client then closes the transport.
	rest := p.rest(t)
	if len(rest) != 1 || rest[0].opcode != wirePong || string(rest[0].payload) != "hi" {
		t.Fatalf("frames after Close = %+v, want one Pong", rest)
	}
	if _, _, err := c.ReadMessage(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ReadMessage after the peer's Close: err = %v, want net.ErrClosed", err)
	}
}

func TestShutdownAfterCloseWrite(t *testing.T) {
	c, p := newScriptedPeer(t)
	errc := make(chan error, 1)
	go func() { errc <- c.CloseWrite(websocket.StatusNormalClosure, "") }()
	if f := p.next(t); f.opcode != wireClose {
		t.Fatalf("first frame = %#x, want Close", f.opcode)
	}
	if err := <-errc; err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	sent := p.send(textFrame("discarded"), normalCloseFrame)
	if err := c.Shutdown(shutdownContext(t), websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("peer send: %v", err)
	}
	if rest := p.rest(t); len(rest) != 0 {
		t.Fatalf("frames after Close = %+v, want none", rest)
	}
}

func TestCloseAfterCloseWriteSendsOneCloseFrame(t *testing.T) {
	for _, closeFn := range []func(*websocket.Conn) error{
		(*websocket.Conn).Close,
		func(c *websocket.Conn) error { return c.CloseWithStatus(websocket.StatusGoingAway, "") },
	} {
		c, p := newScriptedPeer(t)
		errc := make(chan error, 1)
		go func() { errc <- c.CloseWrite(websocket.StatusNormalClosure, "") }()
		if f := p.next(t); f.opcode != wireClose {
			t.Fatalf("first frame = %#x, want Close", f.opcode)
		}
		if err := <-errc; err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		if err := closeFn(c); err != nil {
			t.Fatalf("close after CloseWrite: %v", err)
		}
		if rest := p.rest(t); len(rest) != 0 {
			t.Fatalf("frames after Close = %+v, want none", rest)
		}
	}
}

func TestCloseWriteDrainBoundedByContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, p := newScriptedPeer(t)
		errc := make(chan error, 1)
		go func() { errc <- c.CloseWrite(websocket.StatusNormalClosure, "") }()
		p.next(t)
		if err := <-errc; err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if _, _, err := c.ReadMessage(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReadMessage on a silent peer: err = %v, want DeadlineExceeded", err)
		}
		// The connection is aborted, so the peer sees its side of the pipe end.
		if rest := p.rest(t); len(rest) != 0 {
			t.Fatalf("frames after Close = %+v, want none", rest)
		}
	})
}

// TestCloseWriteConcurrentWrites checks that a Close frame racing with data
// writes is sent once and is the last frame on the wire.
func TestCloseWriteConcurrentWrites(t *testing.T) {
	for range 100 {
		c, p := newScriptedPeer(t)
		writers := make(chan struct{})
		for range 4 {
			go func() {
				defer func() { writers <- struct{}{} }()
				for range 20 { // stays within the peer's frame buffer
					if err := c.WriteMessage(context.Background(), websocket.BinaryMessage, []byte("data")); err != nil {
						return
					}
				}
			}()
		}
		time.Sleep(time.Millisecond)
		if err := c.CloseWrite(websocket.StatusNormalClosure, ""); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		for range 4 {
			<-writers
		}
		c.Close()
		closes := 0
		for _, f := range p.rest(t) {
			switch {
			case f.opcode == wireClose:
				closes++
			case closes > 0:
				t.Fatalf("frame %#x after the Close frame", f.opcode)
			}
		}
		if closes != 1 {
			t.Fatalf("saw %d Close frames, want 1", closes)
		}
	}
}
