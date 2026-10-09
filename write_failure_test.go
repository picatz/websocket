package websocket

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failedWriteError struct{ label string }

func (e *failedWriteError) Error() string { return e.label }

type failedWriteRecorder struct {
	net.Conn
	reader                   *bytes.Reader
	wire                     bytes.Buffer
	calls, closes, deadlines int
	failAt                   int
	prefix                   int
	cause                    error
	shortOnce                bool
}

func (c *failedWriteRecorder) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *failedWriteRecorder) Write(p []byte) (int, error) {
	c.calls++
	if c.calls == max(1, c.failAt) && (c.cause != nil || c.shortOnce) {
		n := min(c.prefix, len(p))
		if c.prefix < 0 {
			n = len(p)
		}
		c.wire.Write(p[:n])
		return n, c.cause
	}
	return c.wire.Write(p)
}
func (c *failedWriteRecorder) Close() error                     { c.closes++; return nil }
func (c *failedWriteRecorder) SetWriteDeadline(time.Time) error { c.deadlines++; return nil }
func (c *failedWriteRecorder) SetReadDeadline(time.Time) error  { return nil }
func (c *failedWriteRecorder) SetDeadline(time.Time) error      { return nil }

type failedWriteExtension struct {
	Extension
	outgoing, enabled, names int
	onEnabled                func()
	call                     func(*Frame) error
}

func (e *failedWriteExtension) Name() string { e.names++; return "test-outgoing" }
func (e *failedWriteExtension) IsEnabled() bool {
	e.enabled++
	if e.onEnabled != nil {
		e.onEnabled()
	}
	return true
}
func (*failedWriteExtension) ProcessIncomingFrame(*Frame) error { return nil }
func (e *failedWriteExtension) ProcessOutgoingFrame(f *Frame) error {
	e.outgoing++
	if e.call != nil {
		return e.call(f)
	}
	return nil
}
func requireWriteCause(t *testing.T, err, errorCause error) {
	t.Helper()
	var concrete *failedWriteError
	if !errors.Is(err, ErrWriteFailed) || !errors.Is(err, errorCause) || !errors.As(err, &concrete) || concrete != errorCause {
		t.Fatalf("lost transport error identity: %v", err)
	}
}
func TestWriteFailureWritePoisonPreventsWireReuse(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, stage := range []string{"header", "payload", "flush"} {
			for _, prefix := range []int{0, 1, -1} {
				t.Run(fmt.Sprintf("server=%v/%s/prefix=%d", server, stage, prefix), func(t *testing.T) {
					cause := &failedWriteError{"synthetic write error"}
					raw := &failedWriteRecorder{reader: bytes.NewReader(failureWire(server, 0x82, []byte("inbound"))), prefix: prefix, cause: cause}
					ext := &failedWriteExtension{}
					c := NewConn(raw, server, []Extension{ext})
					data := []byte("outbound")
					if stage == "header" {
						c.rw.Writer = bufio.NewWriterSize(raw, 1)
					}
					if stage == "payload" {
						data = bytes.Repeat([]byte{'x'}, 8192)
					}
					err := c.WriteMessage(BinaryMessage, data)
					requireWriteCause(t, err, cause)
					if !strings.Contains(err.Error(), map[string]string{"header": "write frame header", "payload": "write frame payload", "flush": "flush data"}[stage]) {
						t.Fatal(err)
					}
					before := bytes.Clone(raw.wire.Bytes())
					if c.writeErr != cause || c.closed.Load() || raw.closes != 0 {
						t.Fatalf("unexpected state: write error=%v closed=%v closes=%d", c.writeErr, c.closed.Load(), raw.closes)
					}
					// The underlying recorder would now accept writes. The buffered writer
					// must never call it again, even after the caller clears a deadline.
					if err := c.SetWriteDeadline(time.Time{}); err != nil {
						t.Fatal(err)
					}
					for _, write := range []func() error{
						func() error { return c.WriteMessage(BinaryMessage, []byte("retry")) },
						func() error { return c.WriteControlFrame(PingMessage, nil) },
						func() error { return c.WriteControlFrame(PongMessage, nil) },
						func() error { return c.WriteControlFrame(CloseMessage, nil) },
					} {
						retryErr := write()
						requireWriteCause(t, retryErr, cause)
						if want := fmt.Sprintf("%v: failed to write frame header: %v", ErrWriteFailed, cause); retryErr.Error() != want {
							t.Fatalf("retry error=%q, want %q", retryErr, want)
						}
					}
					if raw.calls != 1 || !bytes.Equal(before, raw.wire.Bytes()) {
						t.Fatal("poisoned stream was reused")
					}
					// Retries must stop before invoking outgoing extensions.
					if ext.outgoing != 1 || ext.enabled != 1 || ext.names != 0 {
						t.Fatalf("callback count=%d", ext.outgoing)
					}
					if typ, data, err := c.ReadMessage(); typ != BinaryMessage || string(data) != "inbound" || err != nil {
						t.Fatalf("independent read: %v %q %v", typ, data, err)
					}
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}
					if raw.closes != 1 || raw.calls != 1 || ext.outgoing != 1 || raw.deadlines != 1 || !bytes.Equal(before, raw.wire.Bytes()) {
						t.Fatal("Close retried poisoned stream, altered deadlines or invoked extension")
					}
					if _, _, err := c.ReadMessage(); err != io.ErrClosedPipe {
						t.Fatal(err)
					}
					if err := c.WriteMessage(BinaryMessage, nil); err != io.ErrClosedPipe {
						t.Fatal(err)
					}
					if err := c.Close(); err != ErrAlreadyClosed {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestWriteFailureWritePreflightRemainsUsable(t *testing.T) {
	for _, server := range []bool{false, true} {
		raw := &failedWriteRecorder{reader: bytes.NewReader(nil)}
		ext := &failedWriteExtension{}
		c := NewConn(raw, server, []Extension{ext})
		for _, write := range []func() error{
			func() error { return c.WriteMessage(PingMessage, nil) },
			func() error { return c.WriteMessage(TextMessage, []byte{255}) },
			func() error { return c.WriteControlFrame(BinaryMessage, nil) },
			func() error { return c.WriteControlFrame(PingMessage, make([]byte, 126)) },
			func() error { return c.WriteControlFrame(CloseMessage, []byte{1}) },
			func() error { return c.WriteControlFrame(CloseMessage, []byte{3, 237}) },
			func() error { return c.WriteControlFrame(CloseMessage, []byte{3, 232, 255}) },
		} {
			if err := write(); err == nil || errors.Is(err, ErrWriteFailed) {
				t.Fatal(err)
			}
		}
		if raw.calls != 0 || raw.closes != 0 || ext.outgoing != 0 || c.writeErr != nil || c.closed.Load() {
			t.Fatal("preflight performed wire I/O or terminalized")
		}
		cause := errors.New("extension declined this message")
		ext.call = func(*Frame) error { return cause }
		err := c.WriteMessage(BinaryMessage, nil)
		// Current outgoing extension errors use %v, unlike incoming errors. Do not
		// change their wrapping contract while guarding transport errors.
		if err == nil || errors.Is(err, cause) || errors.Is(err, ErrWriteFailed) || !strings.Contains(err.Error(), cause.Error()) {
			t.Fatalf("extension error identity changed: %v", err)
		}
		if raw.calls != 0 || raw.closes != 0 || c.writeErr != nil || c.closed.Load() {
			t.Fatal("extension rejection performed I/O or terminalized")
		}
		ext.call = nil
		if err := c.WriteMessage(BinaryMessage, []byte("ok")); err != nil {
			t.Fatal(err)
		}
		if raw.calls != 1 || ext.outgoing != 2 {
			t.Fatal("not reusable after harmless preflight")
		}
	}
}

type failedWritePipe struct {
	net.Conn
	reads          chan struct{}
	writes, closes atomic.Int64
}

func (c *failedWritePipe) Read(p []byte) (int, error) {
	select {
	case c.reads <- struct{}{}:
	default:
	}
	return c.Conn.Read(p)
}
func (c *failedWritePipe) Write(p []byte) (int, error) { c.writes.Add(1); return c.Conn.Write(p) }
func (c *failedWritePipe) Close() error                { c.closes.Add(1); return c.Conn.Close() }
func failedWriteAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not finish")
		var z T
		return z
	}
}
func failedWritePair(t *testing.T) (*Conn, *failedWritePipe, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := b.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	raw := &failedWritePipe{Conn: a, reads: make(chan struct{}, 16)}
	return NewConn(raw, true, nil), raw, b
}
func failedWriteTimeout(t *testing.T, err error) {
	t.Helper()
	var ne net.Error
	if !errors.Is(err, ErrWriteFailed) || !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatal(err)
	}
}
func TestWriteFailurePartialWriteThenClearAndClose(t *testing.T) {
	c, raw, peer := failedWritePair(t)
	read := make(chan error, 1)
	go func() { _, _, err := c.ReadMessage(); read <- err }()
	failedWriteAwait(t, raw.reads)
	write := make(chan error, 1)
	go func() { write <- c.WriteMessage(BinaryMessage, []byte("partial frame")) }()
	head := make([]byte, 2)
	if _, err := io.ReadFull(peer, head); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(head, []byte{0x82, 13}) {
		t.Fatalf("header=%x", head)
	}
	if err := c.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	failedWriteTimeout(t, failedWriteAwait(t, write))
	if err := c.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	failedWriteTimeout(t, c.WriteMessage(BinaryMessage, []byte("retry")))
	if raw.writes.Load() != 1 || raw.closes.Load() != 0 || c.closed.Load() {
		t.Fatal("cleared deadline permitted reuse or write auto-closed")
	}
	// The blocked read still owns readMu. Close must skip poisoned notification
	// and close the transport, which unblocks that read without waiting on it.
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	if err := failedWriteAwait(t, closed); err != nil {
		t.Fatal(err)
	}
	if err := failedWriteAwait(t, read); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("in-flight read error=%v", err)
	}
	if raw.writes.Load() != 1 {
		t.Fatal("Close appended a frame after partial output")
	}
	if _, _, err := c.ReadMessage(); err != io.ErrClosedPipe {
		t.Fatal(err)
	}
}
func TestWriteFailureRetrySkipsStalledExtension(t *testing.T) {
	for _, callback := range []string{"IsEnabled", "ProcessOutgoingFrame"} {
		t.Run(callback, func(t *testing.T) {
			c, raw, _ := failedWritePair(t)
			read := make(chan error, 1)
			go func() { _, _, err := c.ReadMessage(); read <- err }()
			failedWriteAwait(t, raw.reads)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			ext := &failedWriteExtension{}
			ext.call = func(*Frame) error {
				if ext.outgoing == 2 {
					close(entered)
					<-release
				}
				return nil
			}
			if callback == "IsEnabled" {
				ext.call = nil
				ext.onEnabled = func() {
					if ext.enabled == 2 {
						close(entered)
						<-release
					}
				}
			}
			c.extensions = []Extension{ext}
			if err := c.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			failedWriteTimeout(t, c.WriteMessage(BinaryMessage, nil))
			retry := make(chan error, 1)
			go func() { retry <- c.WriteMessage(BinaryMessage, nil) }()
			failedWriteTimeout(t, failedWriteAwait(t, retry))
			select {
			case <-entered:
				t.Fatal("retry invoked the stalled extension")
			default:
			}
			closed := make(chan error, 1)
			go func() { closed <- c.Close() }()
			if err := failedWriteAwait(t, closed); err != nil {
				t.Fatal(err)
			}
			if err := failedWriteAwait(t, read); err == nil {
				t.Fatal("blocked read not interrupted")
			}
			once.Do(func() { close(release) })
			if raw.writes.Load() != 1 || ext.outgoing != 1 {
				t.Fatal("cleanup reentered callback or transport")
			}
		})
	}
}

func TestWriteFailureShortTransportWriteBecomesSticky(t *testing.T) {
	for _, n := range []int{0, 1} {
		raw := &failedWriteRecorder{reader: bytes.NewReader(nil), prefix: n, shortOnce: true}
		c := NewConn(raw, true, nil)
		for range 3 {
			err := c.WriteMessage(BinaryMessage, []byte("payload"))
			if !errors.Is(err, ErrWriteFailed) || !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}
		}
		if raw.calls != 1 || raw.wire.Len() != n {
			t.Fatal("short write reused transport")
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if raw.calls != 1 {
			t.Fatal("Close wrote on partial stream")
		}
	}
}
func TestWriteFailureFailedControlWritesKeepReadLifetime(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, opcode := range []Opcode{PingMessage, PongMessage, CloseMessage} {
			cause := &failedWriteError{"control transport failure"}
			raw := &failedWriteRecorder{reader: bytes.NewReader(failureWire(server, 0x82, []byte("inbound"))), prefix: 1, cause: cause}
			c := NewConn(raw, server, nil)
			requireWriteCause(t, c.WriteControlFrame(opcode, nil), cause)
			if c.closeSent || c.closed.Load() || c.writeErr == nil || raw.closes != 0 {
				t.Fatalf("failed %v changed lifetime", opcode)
			}
			requireWriteCause(t, c.WriteMessage(BinaryMessage, nil), cause)
			if _, data, err := c.ReadMessage(); err != nil || string(data) != "inbound" {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if raw.calls != 1 || raw.closes != 1 || raw.deadlines != 0 {
				t.Fatal("unexpected cleanup")
			}
		}
	}
}
func TestWriteFailureRetrySkipsMutatingAndFailingExtension(t *testing.T) {
	cause := &failedWriteError{"original transport failure"}
	replacement := errors.New("later extension rejection")
	raw := &failedWriteRecorder{reader: bytes.NewReader(nil), prefix: 1, cause: cause}
	ext := &failedWriteExtension{}
	ext.call = func(f *Frame) error {
		if ext.outgoing > 1 {
			f.Payload[0] = 'X'
			return replacement
		}
		return nil
	}
	c := NewConn(raw, true, []Extension{ext})
	requireWriteCause(t, c.WriteMessage(BinaryMessage, []byte("first")), cause)
	retry := []byte("retry")
	err := c.WriteMessage(BinaryMessage, retry)
	if string(retry) != "retry" || ext.outgoing != 1 || ext.enabled != 1 || ext.names != 0 || raw.calls != 1 {
		t.Fatal("retry invoked an extension or mutated the payload")
	}
	requireWriteCause(t, err, cause)
	if strings.Contains(err.Error(), replacement.Error()) {
		t.Fatalf("retry error=%v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if ext.outgoing != 1 || raw.calls != 1 {
		t.Fatal("cleanup reentered callback or wire")
	}
}

type failedWriteGate struct {
	net.Conn
	entered, release chan struct{}
	calls            int
	cause            error
}

func (c *failedWriteGate) Write(p []byte) (int, error) {
	c.calls++
	if c.calls == 1 {
		close(c.entered)
		<-c.release
	}
	return 1, c.cause
}
func (c *failedWriteGate) Close() error { return nil }
func TestWriteFailureQueuedWriteSkipsExtensions(t *testing.T) {
	cause := &failedWriteError{"first writer failed"}
	raw := &failedWriteGate{entered: make(chan struct{}), release: make(chan struct{}), cause: cause}
	var once sync.Once
	defer once.Do(func() { close(raw.release) })
	ext := &failedWriteExtension{}
	c := NewConn(raw, true, []Extension{ext})
	results := make(chan error, 2)
	go func() { results <- c.WriteMessage(BinaryMessage, []byte("one")) }()
	failedWriteAwait(t, raw.entered)
	started := make(chan struct{})
	go func() { close(started); results <- c.WriteMessage(BinaryMessage, []byte("two")) }()
	failedWriteAwait(t, started)
	once.Do(func() { close(raw.release) })
	for range 2 {
		requireWriteCause(t, failedWriteAwait(t, results), cause)
	}
	if raw.calls != 1 || ext.outgoing != 1 || ext.enabled != 1 || ext.names != 0 {
		t.Fatalf("writes=%d callbacks=%d", raw.calls, ext.outgoing)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFailureValidationPrecedence(t *testing.T) {
	cause := &failedWriteError{"transport failure"}
	raw := &failedWriteRecorder{reader: bytes.NewReader(nil), cause: cause}
	ext := &failedWriteExtension{}
	c := NewConn(raw, true, []Extension{ext})
	requireWriteCause(t, c.WriteMessage(BinaryMessage, nil), cause)
	for _, closed := range []bool{false, true} {
		if closed {
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		}
		checks := []struct {
			write func() error
			want  error
		}{
			{func() error { return c.WriteMessage(PingMessage, nil) }, ErrInvalidOpcode},
			{func() error { return c.WriteMessage(TextMessage, []byte{255}) }, ErrInvalidFrame},
			{func() error { return c.WriteControlFrame(BinaryMessage, nil) }, ErrInvalidOpcode},
			{func() error { return c.WriteControlFrame(PingMessage, make([]byte, 126)) }, ErrPayloadTooLarge},
			{func() error { return c.WriteControlFrame(CloseMessage, []byte{1}) }, ErrInvalidFrame},
		}
		for _, check := range checks {
			if err := check.write(); !errors.Is(err, check.want) || errors.Is(err, cause) {
				t.Fatalf("closed=%v: error=%v, want %v", closed, err, check.want)
			}
		}
		for _, write := range []func() error{
			func() error { return c.WriteMessage(BinaryMessage, nil) },
			func() error { return c.WriteControlFrame(PingMessage, nil) },
			func() error { return c.WriteControlFrame(PongMessage, nil) },
			func() error { return c.WriteControlFrame(CloseMessage, nil) },
		} {
			err := write()
			if closed {
				if err != io.ErrClosedPipe {
					t.Fatal(err)
				}
			} else {
				requireWriteCause(t, err, cause)
			}
		}
	}
	if raw.calls != 1 || ext.outgoing != 1 || ext.enabled != 1 || ext.names != 0 || c.writeErr != cause {
		t.Fatal("retry entered callbacks or changed original cause")
	}
}

func TestWriteFailureAfterSuccessfulClose(t *testing.T) {
	cause := &failedWriteError{"pong transport failure"}
	raw := &failedWriteRecorder{reader: bytes.NewReader(failureWire(true, 0x82, []byte("inbound"))), failAt: 2, cause: cause}
	ext := &failedWriteExtension{}
	c := NewConn(raw, true, []Extension{ext})
	if err := c.WriteControlFrame(CloseMessage, nil); err != nil {
		t.Fatal(err)
	}
	requireWriteCause(t, c.WriteControlFrame(PongMessage, nil), cause)
	if !c.closeSent || c.closed.Load() {
		t.Fatal("successful close state lost")
	}
	if err := c.WriteMessage(BinaryMessage, nil); err != io.ErrClosedPipe {
		t.Fatal(err)
	}
	if err := c.WriteControlFrame(CloseMessage, nil); err != io.ErrClosedPipe {
		t.Fatal(err)
	}
	requireWriteCause(t, c.WriteControlFrame(PingMessage, nil), cause)
	requireWriteCause(t, c.WriteControlFrame(PongMessage, nil), cause)
	if raw.calls != 2 || ext.outgoing != 2 || ext.enabled != 2 {
		t.Fatal("retry reached callback or wire")
	}
	if _, data, err := c.ReadMessage(); err != nil || string(data) != "inbound" {
		t.Fatalf("independent read: %q %v", data, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if raw.calls != 2 || raw.closes != 1 || raw.deadlines != 0 {
		t.Fatal("Close retried failed stream")
	}
}

func TestWriteFailureExtensionCanCloseDuringFirstWrite(t *testing.T) {
	c, raw, _ := failedWritePair(t)
	read := make(chan error, 1)
	go func() { _, _, err := c.ReadMessage(); read <- err }()
	failedWriteAwait(t, raw.reads)
	ext := &failedWriteExtension{}
	ext.call = func(*Frame) error { return c.Close() }
	c.extensions = []Extension{ext}
	write := make(chan error, 1)
	go func() { write <- c.WriteMessage(BinaryMessage, nil) }()
	if err := failedWriteAwait(t, write); !errors.Is(err, ErrWriteFailed) || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if err := failedWriteAwait(t, read); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if ext.outgoing != 1 || ext.enabled != 1 || raw.writes.Load() != 1 || !c.closed.Load() {
		t.Fatal("reentrant Close changed write lifecycle")
	}
}
