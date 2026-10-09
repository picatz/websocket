package websocket_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/picatz/websocket"
)

var _ interface {
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
} = (*websocket.Conn)(nil)

type deadlineCall struct {
	method string
	value  time.Time
}

// This transport deliberately returns the configured error even after Close,
// so the tests detect Conn substituting its own closed-state checks or errors.
type deadlineRecordingConn struct {
	net.Conn
	reader    io.Reader
	written   bytes.Buffer
	calls     []deadlineCall
	setErr    error
	readCalls int
	closes    int
}

func (c *deadlineRecordingConn) Read(p []byte) (int, error) {
	c.readCalls++
	if c.reader == nil {
		return 0, io.EOF
	}
	return c.reader.Read(p)
}
func (c *deadlineRecordingConn) Write(p []byte) (int, error) { return c.written.Write(p) }
func (c *deadlineRecordingConn) Close() error                { c.closes++; return nil }
func (c *deadlineRecordingConn) record(method string, value time.Time) error {
	c.calls = append(c.calls, deadlineCall{method, value})
	return c.setErr
}
func (c *deadlineRecordingConn) SetDeadline(v time.Time) error {
	return c.record("both", v)
}
func (c *deadlineRecordingConn) SetReadDeadline(v time.Time) error {
	return c.record("read", v)
}
func (c *deadlineRecordingConn) SetWriteDeadline(v time.Time) error {
	return c.record("write", v)
}

func TestDeadlineForwarding(t *testing.T) {
	transportErr := errors.New("transport does not support deadlines")
	for _, closed := range []bool{false, true} {
		raw := &deadlineRecordingConn{}
		c := websocket.NewConn(raw, true, nil)
		if closed {
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		}
		reads, writes, closes := raw.readCalls, raw.written.Len(), raw.closes
		for _, setter := range []struct {
			name string
			set  func(time.Time) error
		}{
			{"both", c.SetDeadline},
			{"read", c.SetReadDeadline},
			{"write", c.SetWriteDeadline},
		} {
			for _, value := range []time.Time{{}, time.Unix(123, 456).In(time.FixedZone("test", 3600)), time.Now().Add(time.Hour)} {
				for _, wantErr := range []error{nil, transportErr, io.ErrClosedPipe} {
					raw.calls, raw.setErr = nil, wantErr
					if err := setter.set(value); err != wantErr {
						t.Fatalf("closed=%v %s: error = %v, want identical %v", closed, setter.name, err, wantErr)
					}
					want := deadlineCall{setter.name, value}
					if len(raw.calls) != 1 || raw.calls[0] != want {
						t.Fatalf("closed=%v %s: calls = %v, want exactly %v", closed, setter.name, raw.calls, want)
					}
					if raw.readCalls != reads || raw.written.Len() != writes || raw.closes != closes {
						t.Fatal("deadline setter performed transport I/O or Close")
					}
				}
			}
		}
	}
}

type deadlineObservedConn struct {
	net.Conn
	reads, writes chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
}

func (c *deadlineObservedConn) Read(p []byte) (int, error) {
	select {
	case c.reads <- struct{}{}:
	default:
	}
	return c.Conn.Read(p)
}
func (c *deadlineObservedConn) Write(p []byte) (int, error) {
	select {
	case c.writes <- struct{}{}:
	default:
	}
	return c.Conn.Write(p)
}
func (c *deadlineObservedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func deadlinePipe(t *testing.T, server bool) (*websocket.Conn, *deadlineObservedConn, net.Conn) {
	t.Helper()
	conn, peer := net.Pipe()
	raw := &deadlineObservedConn{Conn: conn, reads: make(chan struct{}, 16), writes: make(chan struct{}, 16), closed: make(chan struct{})}
	t.Cleanup(func() { raw.Close(); peer.Close() })
	// Bound peer-side synchronization too, so a regression cannot hang a test.
	if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return websocket.NewConn(raw, server, nil), raw, peer
}

func deadlineWait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// Run setters separately to detect accidentally taking an I/O lock, while still
// allowing the test's cleanup to close the raw transport after a failed assertion.
func deadlineSet(t *testing.T, setter func(time.Time) error, value time.Time) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- setter(value) }()
	if err := deadlineWait(t, done, "deadline setter"); err != nil {
		t.Fatal(err)
	}
}

func deadlineTimeout(t *testing.T, err error) {
	t.Helper()
	var ne net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("error = %v; want deadline cause and net.Error.Timeout", err)
	}
}

func deadlineTerminal(t *testing.T, c *websocket.Conn) {
	t.Helper()
	if _, data, err := c.ReadMessage(); !errors.Is(err, io.ErrClosedPipe) || data != nil {
		t.Fatalf("read after failure = %x, %v", data, err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after read failure = %v", err)
	}
}

type deadlineMessage struct {
	opcode websocket.Opcode
	data   []byte
	err    error
}

func deadlineRead(c *websocket.Conn) <-chan deadlineMessage {
	done := make(chan deadlineMessage, 1)
	go func() {
		opcode, data, err := c.ReadMessage()
		done <- deadlineMessage{opcode, data, err}
	}()
	return done
}

func deadlineLoopback(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	type upgradeResult struct {
		conn *websocket.Conn
		err  error
	}
	upgraded := make(chan upgradeResult, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		upgraded <- upgradeResult{conn, err}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	result := deadlineWait(t, upgraded, "Upgrade")
	if result.err != nil {
		t.Fatal(result.err)
	}
	t.Cleanup(func() { result.conn.Close() })
	return client, result.conn
}

func deadlineExchange(t *testing.T, from, to *websocket.Conn) {
	t.Helper()
	read := deadlineRead(to)
	write := make(chan error, 1)
	go func() { write <- from.WriteMessage(websocket.BinaryMessage, []byte("live")) }()
	if err := deadlineWait(t, write, "loopback write"); err != nil {
		t.Fatal(err)
	}
	got := deadlineWait(t, read, "loopback read")
	if got.err != nil || got.opcode != websocket.BinaryMessage || string(got.data) != "live" {
		t.Fatalf("message = %v, %q, %v", got.opcode, got.data, got.err)
	}
}

func TestDeadlineDialAndUpgrade(t *testing.T) {
	for _, role := range []string{"Dial", "Upgrade"} {
		t.Run(role, func(t *testing.T) {
			c, peer := deadlineLoopback(t)
			if role == "Upgrade" {
				c, peer = peer, c
			}
			for _, setter := range []struct {
				name string
				set  func(time.Time) error
			}{
				{"both", c.SetDeadline},
				{"read", c.SetReadDeadline},
				{"write", c.SetWriteDeadline},
			} {
				for _, clear := range []bool{true, false} {
					// No I/O occurs between these calls: clearing or refreshing
					// an expired deadline is safe before a WebSocket I/O error.
					deadlineSet(t, setter.set, time.Now().Add(-time.Second))
					var next time.Time
					if !clear {
						next = time.Now().Add(time.Minute)
					}
					deadlineSet(t, setter.set, next)
					deadlineExchange(t, c, peer)
					deadlineExchange(t, peer, c)
				}
			}
			deadlineSet(t, c.SetReadDeadline, time.Now().Add(-time.Second))
			deadlineTimeout(t, deadlineWait(t, deadlineRead(c), "loopback timeout").err)
			deadlineTerminal(t, c)
		})
	}
}

func TestDeadlineInterruptsPartialPayloadRead(t *testing.T) {
	for _, shared := range []bool{false, true} {
		c, raw, peer := deadlinePipe(t, false)
		read := deadlineRead(c)
		deadlineWait(t, raw.reads, "initial read")
		// The frame declares two bytes, but only one payload byte arrives.
		if _, err := peer.Write([]byte{0x82, 2, 'a'}); err != nil {
			t.Fatal(err)
		}
		deadlineWait(t, raw.reads, "remaining payload read")
		setter := c.SetReadDeadline
		if shared {
			setter = c.SetDeadline
		}
		deadlineSet(t, setter, time.Now().Add(-time.Second))
		result := deadlineWait(t, read, "partial payload timeout")
		deadlineTimeout(t, result.err)
		if result.data != nil {
			t.Fatalf("partial payload exposed after error: %x", result.data)
		}
		deadlineWait(t, raw.closed, "transport abort")
		deadlineTerminal(t, c)
	}
}

func TestDeadlineRefreshPendingRead(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, clear := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				c, _, peer := deadlinePipe(t, false)
				setter := c.SetReadDeadline
				if shared {
					setter = c.SetDeadline
				}
				deadlineSet(t, setter, time.Now().Add(time.Second))
				read := deadlineRead(c)
				synctest.Wait()
				// Advance only the bubble's fake clock. Refresh while the read
				// is blocked, before its original deadline can expire.
				time.Sleep(500 * time.Millisecond)
				var next time.Time
				if !clear {
					next = time.Now().Add(2 * time.Second)
				}
				deadlineSet(t, setter, next)
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case result := <-read:
					t.Fatalf("shared=%v clear=%v: original deadline still ended read: %+v", shared, clear, result)
				default:
				}
				if _, err := peer.Write([]byte{0x82, 1, 'x'}); err != nil {
					t.Fatal(err)
				}
				got := deadlineWait(t, read, "refreshed read")
				if got.err != nil || got.opcode != websocket.BinaryMessage || string(got.data) != "x" {
					t.Fatalf("message after refresh = %+v", got)
				}
			})
		}
	}
}

func TestDeadlineWriteFailureRequiresClose(t *testing.T) {
	c, raw, peer := deadlinePipe(t, true)
	read := deadlineRead(c)
	deadlineWait(t, raw.reads, "concurrent read")
	write := make(chan error, 1)
	go func() { write <- c.WriteMessage(websocket.BinaryMessage, []byte("payload")) }()
	// Consume only the header; the caller's write remains blocked on payload.
	if _, err := io.ReadFull(peer, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	deadlineSet(t, c.SetWriteDeadline, time.Now().Add(-time.Second))
	err := deadlineWait(t, write, "payload write timeout")
	deadlineTimeout(t, err)
	if !errors.Is(err, websocket.ErrWriteFailed) {
		t.Fatalf("write error = %v; want ErrWriteFailed", err)
	}
	select {
	case <-raw.closed:
		t.Fatal("caller write timeout unexpectedly closed the transport")
	default:
	}
	// The reader is still usable. Send an independently encoded masked frame.
	if _, err := peer.Write([]byte{0x82, 0x80, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if result := deadlineWait(t, read, "independent read"); result.err != nil || result.opcode != websocket.BinaryMessage {
		t.Fatalf("read after write timeout = %+v", result)
	}
	read = deadlineRead(c)
	deadlineWait(t, raw.reads, "next read")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	deadlineWait(t, raw.closed, "caller Close")
	if result := deadlineWait(t, read, "read interrupted by Close"); result.err == nil {
		t.Fatal("read succeeded after Close")
	}
}

func TestDeadlineAutomaticPong(t *testing.T) {
	for _, kind := range []string{"read", "write", "both", "close"} {
		t.Run(kind, func(t *testing.T) {
			c, raw, peer := deadlinePipe(t, false)
			read := deadlineRead(c)
			if _, err := peer.Write([]byte{0x89, 0}); err != nil {
				t.Fatal(err)
			}
			deadlineWait(t, raw.writes, "automatic Pong write")
			switch kind {
			case "read":
				deadlineSet(t, c.SetReadDeadline, time.Now().Add(-time.Second))
				// An expired read deadline must still allow the blocked Pong
				// to finish. Draining it proves this without a sleep-only check.
				var pong [6]byte // Empty masked client control frame.
				if _, err := io.ReadFull(peer, pong[:]); err != nil {
					t.Fatal(err)
				}
				if pong[0] != 0x8a || pong[1] != 0x80 {
					t.Fatalf("automatic Pong = %x", pong)
				}
			case "write":
				deadlineSet(t, c.SetWriteDeadline, time.Now().Add(-time.Second))
			case "both":
				deadlineSet(t, c.SetDeadline, time.Now().Add(-time.Second))
			case "close":
				closed := make(chan error, 1)
				go func() { closed <- c.Close() }()
				if err := deadlineWait(t, closed, "concurrent Close"); err != nil {
					t.Fatal(err)
				}
			}
			err := deadlineWait(t, read, "ReadMessage after Pong").err
			if kind != "close" {
				deadlineTimeout(t, err)
			}
			if got, want := errors.Is(err, websocket.ErrWriteFailed), kind != "read"; got != want {
				t.Fatalf("error = %v; ErrWriteFailed = %v, want %v", err, got, want)
			}
			deadlineWait(t, raw.closed, "Pong failure abort")
			deadlineTerminal(t, c)
		})
	}
}

func TestDeadlineConcurrentSettersAndClose(t *testing.T) {
	for range 20 {
		c, raw, _ := deadlinePipe(t, true)
		read := deadlineRead(c)
		write := make(chan error, 1)
		go func() { write <- c.WriteMessage(websocket.BinaryMessage, []byte("blocked")) }()
		deadlineWait(t, raw.reads, "concurrent read")
		deadlineWait(t, raw.writes, "concurrent write")
		start := make(chan struct{})
		done := make(chan error, 4)
		for _, setter := range []func(time.Time) error{c.SetDeadline, c.SetReadDeadline, c.SetWriteDeadline} {
			go func() {
				<-start
				for i := range 50 {
					var value time.Time
					if i%2 == 0 {
						value = time.Now().Add(time.Minute)
					}
					if err := setter(value); err != nil && !errors.Is(err, io.ErrClosedPipe) {
						done <- err
						return
					}
				}
				done <- nil
			}()
		}
		go func() { <-start; done <- c.Close() }()
		close(start)
		for range 4 {
			if err := deadlineWait(t, done, "setters and Close"); err != nil {
				t.Fatal(err)
			}
		}
		if err := deadlineWait(t, write, "closed writer"); !errors.Is(err, websocket.ErrWriteFailed) {
			t.Fatalf("write after concurrent Close = %v", err)
		}
		if result := deadlineWait(t, read, "closed reader"); result.err == nil {
			t.Fatal("read succeeded after concurrent Close")
		}
	}
}

func TestDeadlineNormalCloseOverridesWriteDeadline(t *testing.T) {
	for _, callerDeadline := range []time.Time{{}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)} {
		raw := &deadlineRecordingConn{}
		c := websocket.NewConn(raw, true, nil)
		readDeadline := time.Now().Add(time.Minute)
		deadlineSet(t, c.SetReadDeadline, readDeadline)
		deadlineSet(t, c.SetWriteDeadline, callerDeadline)
		before := time.Now()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		if len(raw.calls) != 3 || raw.calls[2].method != "write" {
			t.Fatalf("Close deadline calls = %v", raw.calls)
		}
		got := raw.calls[2].value
		if got.Before(before.Add(time.Second)) || got.After(after.Add(time.Second)) {
			t.Fatalf("Close deadline = %v; want now + one second", got)
		}
		if raw.closes != 1 || !bytes.Equal(raw.written.Bytes(), []byte{0x88, 2, 3, 232}) {
			t.Fatalf("Close: transport closes = %d, wire = %x", raw.closes, raw.written.Bytes())
		}
	}
}

func TestDeadlineFailurePreservesCallerDeadlines(t *testing.T) {
	raw := &deadlineRecordingConn{reader: bytes.NewReader([]byte{0x83, 0x80})}
	c := websocket.NewConn(raw, true, nil)
	for _, setter := range []func(time.Time) error{c.SetDeadline, c.SetReadDeadline, c.SetWriteDeadline} {
		deadlineSet(t, setter, time.Now().Add(-time.Second))
	}
	before := append([]deadlineCall(nil), raw.calls...)
	if _, _, err := c.ReadMessage(); err != websocket.ErrInvalidOpcode {
		t.Fatalf("protocol failure = %v", err)
	}
	if len(raw.calls) != len(before) {
		t.Fatalf("failure changed deadlines: before = %v, after = %v", before, raw.calls)
	}
	for i := range before {
		if raw.calls[i] != before[i] {
			t.Fatal("failure changed a caller deadline")
		}
	}
	if raw.closes != 1 || !bytes.Equal(raw.written.Bytes(), []byte{0x88, 2, 3, 234}) {
		t.Fatalf("failure cleanup: closes = %d, wire = %x", raw.closes, raw.written.Bytes())
	}
	deadlineTerminal(t, c)
}
