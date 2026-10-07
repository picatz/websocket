package websocket

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestCloseSendsCloseFrame(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	conn := NewConn(raw, true, nil)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw.written.Bytes(), []byte{0x88, 2, 0x03, 0xe8}) {
		t.Fatalf("Close wrote %x, want normal close frame", raw.written.Bytes())
	}
	if !errors.Is(conn.Close(), ErrAlreadyClosed) {
		t.Fatal("repeated Close must report ErrAlreadyClosed")
	}
}

func TestConcurrentCloseReadWrite(t *testing.T) {
	for range 100 {
		c := frameConn(nil, true, 0)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, fn := range []func(){func() { c.ReadMessage() }, func() { c.WriteMessage(BinaryMessage, nil) }, func() { c.Close() }} {
			wg.Go(func() { <-start; fn() })
		}
		close(start)
		wg.Wait()
	}
}

func TestCloseUnblocksStalledWriter(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	defer raw.Close()
	c := NewConn(raw, true, nil)
	result := make(chan error, 1)
	go func() { result <- c.WriteMessage(BinaryMessage, bytes.Repeat([]byte{'x'}, 8192)) }()
	// Read only the header. The writer is still blocked on its payload.
	var header [4]byte
	if _, err := io.ReadFull(peer, header[:]); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		peer.Close()
		t.Fatal("Close did not unblock stalled writer")
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("writer remains blocked")
	}
}

type payloadObserver struct {
	source, want []byte
	t            *testing.T
	out          bytes.Buffer
}

func (w *payloadObserver) Write(p []byte) (int, error) {
	if !bytes.Equal(w.source, w.want) {
		w.t.Error("caller payload was temporarily masked during WriteMessage")
	}
	return w.out.Write(p)
}
func TestWriteMessagePreservesPayloadDuringWrite(t *testing.T) {
	data := []byte("shared payload")
	observer := &payloadObserver{source: data, want: bytes.Clone(data), t: t}
	c := &Conn{rw: bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(nil)), bufio.NewWriter(observer))}
	if err := c.WriteMessage(BinaryMessage, data); err != nil {
		t.Fatal(err)
	}
}
