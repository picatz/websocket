package websocket

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Construct peer bytes independently of the production encoder, including masks.
func failureWire(server bool, first byte, payload []byte) []byte {
	b := []byte{first, byte(len(payload))}
	if server {
		b[1] |= 0x80
		b = append(b, 0x11, 0x22, 0x33, 0x44)
		for i, v := range payload {
			b = append(b, v^[]byte{0x11, 0x22, 0x33, 0x44}[i&3])
		}
	} else {
		b = append(b, payload...)
	}
	return b
}

type failureMemoryConn struct {
	memoryConn
	closes        int
	deadlineCalls int
}

func (c *failureMemoryConn) Close() error                     { c.closes++; return nil }
func (c *failureMemoryConn) SetWriteDeadline(time.Time) error { c.deadlineCalls++; return nil }

func failureClosePayload(t *testing.T, wire []byte, server bool) []byte {
	t.Helper()
	if len(wire) < 2 || wire[0] != 0x88 || (wire[1]&0x80 != 0) == server {
		t.Fatalf("invalid close encoding: %x", wire)
	}
	n := int(wire[1] & 127)
	header := 2
	if !server {
		header += 4
	}
	if n > 125 || len(wire) != header+n {
		t.Fatalf("expected exactly one close: %x", wire)
	}
	p := bytes.Clone(wire[header:])
	if !server {
		for i := range p {
			p[i] ^= wire[2+i&3]
		}
	}
	return p
}

func TestReadFailureStatusAndTerminalState(t *testing.T) {
	for _, server := range []bool{false, true} {
		prefix := func(b byte, p []byte) []byte { return failureWire(server, b, p) }
		header := func(b, n byte) []byte {
			if server {
				n |= 128
			}
			return []byte{b, n}
		}
		for _, tc := range []struct {
			name  string
			wire  []byte
			limit int
			cause error
			code  uint16
			exact bool
		}{
			{"text UTF8", prefix(0x81, []byte{255}), 0, ErrInvalidFrame, 1007, true},
			{"fragment UTF8", append(prefix(0x01, []byte{0xc2}), prefix(0x80, []byte{'a'})...), 0, ErrInvalidFrame, 1007, true},
			{"close reason", prefix(0x88, []byte{3, 232, 255}), 0, ErrInvalidFrame, 1007, true},
			{"bad status before reason", prefix(0x88, []byte{3, 237, 255}), 0, ErrInvalidFrame, 1002, true},
			{"one byte close", header(0x88, 1), 0, ErrInvalidFrame, 1002, true},
			{"control oversize", header(0x89, 126), 0, ErrPayloadTooLarge, 1002, false},
			{"control fragment", header(0x09, 0), 0, ErrControlFrameFragment, 1002, true},
			{"opcode", header(0x83, 0), 0, ErrInvalidOpcode, 1002, true},
			{"RSV", header(0xc2, 0), 0, ErrUnsupportedExtensions, 1002, true},
			{"continuation", header(0x80, 0), 0, ErrUnexpectedContinuation, 1002, true},
			{"new data", append(prefix(0x02, nil), header(0x81, 0)...), 0, ErrUnexpectedFrame, 1002, true},
			{"nonminimal16", append(header(0x82, 126), 0, 125), 0, ErrInvalidFrame, 1002, true},
			{"nonminimal64", append(header(0x82, 127), 0, 0, 0, 0, 0, 0, 255, 255), 0, ErrInvalidFrame, 1002, true},
			{"highbit", append(header(0x82, 127), 128, 0, 0, 0, 0, 0, 0, 0), 0, ErrInvalidFrame, 1002, true},
			{"frame limit", header(0x82, 2), 1, ErrPayloadTooLarge, 1009, true},
			{"message limit", append(prefix(0x02, []byte{'a'}), header(0x80, 1)...), 1, ErrPayloadTooLarge, 1009, true},
		} {
			t.Run(strconv.FormatBool(server)+"/"+tc.name, func(t *testing.T) {
				raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(append(tc.wire, prefix(0x81, []byte("late"))...))}}
				c := NewConn(raw, server, nil, WithMaxBytes(tc.limit))
				_, data, err := c.ReadMessage()
				if !errors.Is(err, tc.cause) || (tc.exact && err != tc.cause) || data != nil {
					t.Fatalf("first read: %x %v", data, err)
				}
				if raw.closes != 1 || raw.deadlineCalls != 0 {
					t.Fatalf("cleanup closes/deadlines=%d/%d", raw.closes, raw.deadlineCalls)
				}
				payload := failureClosePayload(t, raw.written.Bytes(), server)
				if len(payload) != 2 || binary.BigEndian.Uint16(payload) != tc.code {
					t.Fatalf("status payload=%x want %d", payload, tc.code)
				}
				before := bytes.Clone(raw.written.Bytes())
				for range 3 {
					if _, p, e := c.ReadMessage(); e != io.ErrClosedPipe || p != nil {
						t.Fatalf("later read %x %v", p, e)
					}
					for _, write := range []func() error{func() error { return c.WriteMessage(TextMessage, []byte("late")) }, func() error { return c.WriteControlFrame(PingMessage, nil) }, func() error { return c.WriteControlFrame(CloseMessage, nil) }} {
						if e := write(); e != io.ErrClosedPipe {
							t.Fatal(e)
						}
					}
					if e := c.Close(); e != ErrAlreadyClosed {
						t.Fatal(e)
					}
				}
				if raw.closes != 1 || !bytes.Equal(before, raw.written.Bytes()) {
					t.Fatal("I/O after terminal failure")
				}
			})
		}
	}
}

func TestReadFailureMaskingStatus(t *testing.T) {
	for _, server := range []bool{false, true} {
		raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(failureWire(!server, 0x82, nil))}}
		_, _, err := NewConn(raw, server, nil).ReadMessage()
		want := ErrMaskedFrame
		if server {
			want = ErrUnmaskedFrame
		}
		if err != want || binary.BigEndian.Uint16(failureClosePayload(t, raw.written.Bytes(), server)) != 1002 {
			t.Fatal(err)
		}
	}
}

func TestIncomingCloseStatusBoundaries(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, code := range []uint16{0, 999, 1000, 1001, 1002, 1003, 1004, 1005, 1006, 1007, 1010, 1011, 1012, 1013, 1014, 1015, 1016, 1100, 2000, 2999, 3000, 4999, 5000, 65535} {
			t.Run(fmt.Sprintf("%v/%d", server, code), func(t *testing.T) {
				p := []byte{byte(code >> 8), byte(code)}
				raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(failureWire(server, 0x88, p))}}
				c := NewConn(raw, server, nil)
				_, _, err := c.ReadMessage()
				invalid := code < 1000 || code >= 5000 || code == 1004 || code == 1005 || code == 1006 || (code >= 1015 && code < 3000)
				want := p
				if invalid {
					if err != ErrInvalidFrame {
						t.Fatal(err)
					}
					want = []byte{3, 234}
				} else if err != io.EOF {
					t.Fatal(err)
				}
				if got := failureClosePayload(t, raw.written.Bytes(), server); !bytes.Equal(got, want) {
					t.Fatalf("close payload=%x want %x", got, want)
				}
			})
		}
		for _, p := range [][]byte{nil, append([]byte{3, 232}, bytes.Repeat([]byte{'x'}, 123)...)} {
			raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(failureWire(server, 0x88, p))}}
			if _, _, err := NewConn(raw, server, nil).ReadMessage(); err != io.EOF {
				t.Fatal(err)
			}
			if got := failureClosePayload(t, raw.written.Bytes(), server); !bytes.Equal(got, p) {
				t.Fatalf("echo=%x want %x", got, p)
			}
		}
	}
}

type failureExtension struct {
	enabled  bool
	cause    error
	outgoing int
}

func (e *failureExtension) Name() string                      { return "failure-test" }
func (e *failureExtension) Offer() string                     { return "failure-test" }
func (e *failureExtension) Negotiate(string) error            { return nil }
func (e *failureExtension) IsEnabled() bool                   { return e.enabled }
func (e *failureExtension) ProcessIncomingFrame(*Frame) error { return e.cause }
func (e *failureExtension) ProcessOutgoingFrame(*Frame) error {
	e.outgoing++
	panic("automatic failure called outgoing extension")
}

func TestReadFailureCallbackProvenance(t *testing.T) {
	for _, cause := range []error{io.EOF, ErrInvalidFrame, ErrPayloadTooLarge, os.ErrDeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			ext := &failureExtension{enabled: true, cause: fmt.Errorf("custom: %w", cause)}
			raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader([]byte{0x82, 0})}}
			c := NewConn(raw, false, []Extension{ext})
			_, _, err := c.ReadMessage()
			want := "extension failure-test failed to process incoming frame: " + ext.cause.Error()
			if !errors.Is(err, cause) || err.Error() != want || raw.closes != 1 || raw.written.Len() != 0 || ext.outgoing != 0 {
				t.Fatalf("error=%v wire=%x closes=%d outgoing=%d", err, raw.written.Bytes(), raw.closes, ext.outgoing)
			}
		})
	}
	for _, enabled := range []bool{false, true} {
		ext := &failureExtension{enabled: enabled}
		raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader([]byte{0x81, 1, 255})}}
		_, _, err := NewConn(raw, false, []Extension{ext}).ReadMessage()
		if err != ErrInvalidFrame || ext.outgoing != 0 {
			t.Fatal(err)
		}
		if enabled && raw.written.Len() != 0 {
			t.Fatal("unsafe custom encoding")
		}
		if !enabled && binary.BigEndian.Uint16(failureClosePayload(t, raw.written.Bytes(), false)) != 1007 {
			t.Fatal("disabled extension prevented notification")
		}
	}
	for _, opcode := range []byte{0x89, 0x8a} {
		cause := fmt.Errorf("handler: %w", ErrInvalidFrame)
		raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader([]byte{opcode, 0})}}
		c := NewConn(raw, false, nil)
		c.SetPingHandler(func(string) error { return cause })
		c.SetPongHandler(func(string) error { return cause })
		if _, _, err := c.ReadMessage(); err != cause || raw.written.Len() != 0 || raw.closes != 1 {
			t.Fatalf("handler error=%v", err)
		}
	}
}

func TestReadFailureBuiltInDecodedLimitDiagnostic(t *testing.T) {
	pmd := NewPerMessageDeflateExtension().(*perMessageDeflate)
	pmd.enabled = true
	f := &Frame{Final: true, Opcode: TextMessage, Payload: bytes.Repeat([]byte{'a'}, 2048)}
	if err := pmd.ProcessOutgoingFrame(f); err != nil {
		t.Fatal(err)
	}
	raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(failureWire(false, 0xc1, f.Payload))}}
	c := NewConn(raw, false, []Extension{pmd}, WithMaxBytes(128))
	_, _, err := c.ReadMessage()
	if !errors.Is(err, ErrPayloadTooLarge) || err.Error() != "extension permessage-deflate failed to process incoming frame: "+ErrPayloadTooLarge.Error() {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(failureClosePayload(t, raw.written.Bytes(), false)) != 1009 {
		t.Fatal("wrong decoded limit code")
	}
}

// Read uses buffered invalid input, while writes exercise real pipe deadlines.
type failurePipeConn struct {
	net.Conn
	input     *bytes.Reader
	started   chan struct{}
	startOnce sync.Once
	closes    atomic.Int32
	setters   atomic.Int32
}

func (c *failurePipeConn) Read(p []byte) (int, error) { return c.input.Read(p) }
func (c *failurePipeConn) Write(p []byte) (int, error) {
	c.startOnce.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}
func (c *failurePipeConn) Close() error { c.closes.Add(1); return c.Conn.Close() }
func (c *failurePipeConn) SetWriteDeadline(d time.Time) error {
	c.setters.Add(1)
	return c.Conn.SetWriteDeadline(d)
}

func failureAwait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not return")
		return nil
	}
}
func TestReadFailurePreservesTransportDeadlines(t *testing.T) {
	for _, delay := range []time.Duration{-time.Second, 40 * time.Millisecond, 0, 10 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			transport, peer := net.Pipe()
			defer peer.Close()
			defer transport.Close()
			deadline := time.Time{}
			if delay != 0 {
				deadline = time.Now().Add(delay)
			}
			if err := transport.SetWriteDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			raw := &failurePipeConn{Conn: transport, input: bytes.NewReader([]byte{0x81, 1, 255}), started: make(chan struct{})}
			c := NewConn(raw, false, nil)
			start := time.Now()
			_, _, err := c.ReadMessage()
			elapsed := time.Since(start)
			if err != ErrInvalidFrame || raw.setters.Load() != 0 || raw.closes.Load() != 1 {
				t.Fatalf("error=%v setters=%d closes=%d", err, raw.setters.Load(), raw.closes.Load())
			}
			if elapsed > 2*time.Second || (delay > 0 && delay < time.Second && elapsed > 500*time.Millisecond) {
				t.Fatalf("deadline extended: %s", elapsed)
			}
			if (delay == 0 || delay > time.Second) && elapsed < 800*time.Millisecond {
				t.Fatalf("did not exercise watchdog: %s", elapsed)
			}
		})
	}
}

func TestConcurrentCloseCancelsFailureNotification(t *testing.T) {
	for range 50 {
		transport, peer := net.Pipe()
		raw := &failurePipeConn{Conn: transport, input: bytes.NewReader([]byte{0x81, 1, 255}), started: make(chan struct{})}
		c := NewConn(raw, false, nil)
		done := make(chan error, 1)
		finished := make(chan struct{})
		t.Cleanup(func() { transport.Close(); peer.Close(); <-finished })
		go func() { defer close(finished); _, _, err := c.ReadMessage(); done <- err }()
		select {
		case <-raw.started:
		case <-time.After(2 * time.Second):
			t.Fatal("notification never started")
		}
		if _, _, err := c.ReadMessage(); err != io.ErrClosedPipe {
			t.Fatal(err)
		}
		start := time.Now()
		if err := c.Close(); err != ErrAlreadyClosed {
			t.Fatal(err)
		}
		if err := failureAwait(t, done); err != ErrInvalidFrame {
			t.Fatal(err)
		}
		if time.Since(start) > 500*time.Millisecond || raw.closes.Load() != 1 {
			t.Fatal("Close did not promptly cancel notification")
		}
		peer.Close()
		transport.Close()
	}
}

func TestReadFailureAbortsStalledDataWriter(t *testing.T) {
	for _, server := range []bool{false, true} {
		transport, peer := net.Pipe()
		defer peer.Close()
		defer transport.Close()
		raw := &failurePipeConn{Conn: transport, input: bytes.NewReader(failureWire(server, 0x81, []byte{255})), started: make(chan struct{})}
		c := NewConn(raw, server, nil)
		done := make(chan error, 1)
		finished := make(chan struct{})
		t.Cleanup(func() { transport.Close(); peer.Close(); <-finished })
		payload := bytes.Repeat([]byte{'a'}, 8192)
		go func() { defer close(finished); done <- c.WriteMessage(BinaryMessage, payload) }()
		if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		// Consume only the independently known data header, so payload is stalled.
		header := make([]byte, 4)
		if !server {
			header = make([]byte, 8)
		}
		if _, err := io.ReadFull(peer, header); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, _, err := c.ReadMessage()
		if err != ErrInvalidFrame || time.Since(start) > 500*time.Millisecond {
			t.Fatalf("failure=%v", err)
		}
		if err := failureAwait(t, done); !errors.Is(err, ErrWriteFailed) || !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("write error=%v", err)
		}
		rest, e := io.ReadAll(peer)
		if e != nil || len(rest) != 0 {
			t.Fatalf("bytes appended after partial frame: %x %v", rest, e)
		}
		if raw.closes.Load() != 1 || !bytes.Equal(payload, bytes.Repeat([]byte{'a'}, 8192)) {
			t.Fatal("abort/immutability regression")
		}
	}
}

type failureFaultConn struct {
	failureMemoryConn
	n      int
	cause  error
	writes int
}

func (c *failureFaultConn) Write(p []byte) (int, error) {
	c.writes++
	n := c.n
	if n > len(p) {
		n = len(p)
	}
	c.written.Write(p[:n])
	return n, c.cause
}
func TestReadFailureNeverRetriesPoisonedWriter(t *testing.T) {
	for _, size := range []int{0, 8192} {
		for _, n := range []int{0, 1} {
			for _, cause := range []error{io.ErrClosedPipe, os.ErrDeadlineExceeded, nil} {
				t.Run(fmt.Sprintf("%d/%d/%v", size, n, cause), func(t *testing.T) {
					raw := &failureFaultConn{failureMemoryConn: failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader([]byte{0x81, 1, 255})}}, n: n, cause: cause}
					c := NewConn(raw, false, nil)
					err := c.WriteMessage(BinaryMessage, bytes.Repeat([]byte{'a'}, size))
					if !errors.Is(err, ErrWriteFailed) || (cause != nil && !errors.Is(err, cause)) {
						t.Fatalf("write error=%v", err)
					}
					before := bytes.Clone(raw.written.Bytes())
					calls := raw.writes
					if _, _, err := c.ReadMessage(); err != ErrInvalidFrame {
						t.Fatal(err)
					}
					c.Close()
					if calls != raw.writes || !bytes.Equal(before, raw.written.Bytes()) || raw.closes != 1 {
						t.Fatal("reused poisoned stream")
					}
				})
			}
		}
	}
	// A sticky error encountered while writing a new header is also retained.
	raw := &failureFaultConn{failureMemoryConn: failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(nil)}}, cause: io.ErrClosedPipe}
	c := NewConn(raw, true, nil)
	c.rw.Writer = bufio.NewWriterSize(raw, 16)
	c.WriteMessage(BinaryMessage, bytes.Repeat([]byte{'a'}, 32))
	if err := c.WriteMessage(BinaryMessage, nil); !errors.Is(err, ErrWriteFailed) || !errors.Is(err, io.ErrClosedPipe) || !strings.Contains(err.Error(), "header") {
		t.Fatal(err)
	}
}

func TestReadFailureTransportEOFIsTerminal(t *testing.T) {
	for _, wire := range [][]byte{nil, {0x82}, {0x82, 1}} {
		raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(wire)}}
		c := NewConn(raw, false, nil)
		_, _, err := c.ReadMessage()
		if len(wire) == 0 && err != io.EOF {
			t.Fatal(err)
		}
		if len(wire) > 0 && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
		if raw.closes != 1 || raw.written.Len() != 0 {
			t.Fatal("transport failure manufactured close code")
		}
	}
}

func TestReadFailureKeepsCauseWhenPeerCannotReceive(t *testing.T) {
	transport, peer := net.Pipe()
	peer.Close()
	defer transport.Close()
	raw := &failurePipeConn{Conn: transport, input: bytes.NewReader([]byte{0x81, 1, 255}), started: make(chan struct{})}
	c := NewConn(raw, false, nil)
	if _, _, err := c.ReadMessage(); err != ErrInvalidFrame {
		t.Fatal(err)
	}
	if raw.closes.Load() != 1 || raw.setters.Load() != 0 {
		t.Fatal("unexpected failure cleanup")
	}
}

func TestReadFailureAfterExplicitCloseDoesNotSendAnother(t *testing.T) {
	for _, server := range []bool{false, true} {
		raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(failureWire(server, 0x81, []byte{255}))}}
		c := NewConn(raw, server, nil)
		if err := c.WriteControlFrame(CloseMessage, []byte{3, 232}); err != nil {
			t.Fatal(err)
		}
		before := bytes.Clone(raw.written.Bytes())
		if _, _, err := c.ReadMessage(); err != ErrInvalidFrame {
			t.Fatal(err)
		}
		if raw.closes != 1 || !bytes.Equal(before, raw.written.Bytes()) {
			t.Fatal("duplicate close after explicit reservation")
		}
	}
}

func TestReadFailurePlatformLimitStatus(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("32-bit declared-size boundary")
	}
	wire := []byte{0x82, 127, 0, 0, 0, 0, 128, 0, 0, 0}
	raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(wire)}}
	if _, _, err := NewConn(raw, false, nil).ReadMessage(); err != ErrPayloadTooLarge {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(failureClosePayload(t, raw.written.Bytes(), false)) != 1009 {
		t.Fatal("platform limit status")
	}
}

type unsupportedDeadlineConn struct{ *failurePipeConn }

func (c *unsupportedDeadlineConn) SetWriteDeadline(time.Time) error {
	c.setters.Add(1)
	return errors.New("deadlines unsupported")
}

func TestReadFailureDoesNotRequireDeadlineSupport(t *testing.T) {
	transport, peer := net.Pipe()
	defer peer.Close()
	defer transport.Close()
	raw := &unsupportedDeadlineConn{&failurePipeConn{Conn: transport, input: bytes.NewReader([]byte{0x81, 1, 255}), started: make(chan struct{})}}
	start := time.Now()
	if _, _, err := NewConn(raw, false, nil).ReadMessage(); err != ErrInvalidFrame {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 800*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("watchdog duration=%s", elapsed)
	}
	if raw.setters.Load() != 0 || raw.closes.Load() != 1 {
		t.Fatal("cleanup depended on unsupported deadlines")
	}
}

type joinedAbortConn struct {
	*failurePipeConn
	entered  chan struct{}
	release  chan struct{}
	returned atomic.Bool
}

func (c *joinedAbortConn) Close() error {
	err := c.failurePipeConn.Close() // Unblock the synchronous notification first.
	close(c.entered)
	<-c.release
	c.returned.Store(true)
	return err
}

func TestReadFailureJoinsRunningAbortWatchdog(t *testing.T) {
	transport, peer := net.Pipe()
	raw := &joinedAbortConn{failurePipeConn: &failurePipeConn{Conn: transport, input: bytes.NewReader([]byte{0x81, 1, 255}), started: make(chan struct{})}, entered: make(chan struct{}), release: make(chan struct{})}
	c := NewConn(raw, false, nil)
	done := make(chan error, 1)
	finished := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(raw.release) }); transport.Close(); peer.Close(); <-finished })
	go func() { defer close(finished); _, _, err := c.ReadMessage(); done <- err }()
	// No peer reads and no deadline exists. Only the watchdog can end the write.
	select {
	case <-raw.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not abort")
	}
	select {
	case err := <-done:
		t.Fatalf("read returned before abort callback completed: %v", err)
	default:
	}
	release.Do(func() { close(raw.release) })
	if err := failureAwait(t, done); err != ErrInvalidFrame {
		t.Fatal(err)
	}
	if !raw.returned.Load() || raw.closes.Load() != 1 || raw.setters.Load() != 0 {
		t.Fatal("abort callback not joined exactly once")
	}
}

func TestReadFailurePartialNotificationIsNeverRetried(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, n := range []int{0, 1, 2, 3, 4} {
			raw := &failureFaultConn{failureMemoryConn: failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(failureWire(server, 0x81, []byte{255}))}}, n: n, cause: io.ErrClosedPipe}
			c := NewConn(raw, server, nil)
			if _, _, err := c.ReadMessage(); err != ErrInvalidFrame {
				t.Fatal(err)
			}
			before := bytes.Clone(raw.written.Bytes())
			if err := c.Close(); err != ErrAlreadyClosed {
				t.Fatal(err)
			}
			if err := c.WriteControlFrame(CloseMessage, nil); err != io.ErrClosedPipe {
				t.Fatal(err)
			}
			if raw.writes != 1 || raw.closes != 1 || !bytes.Equal(before, raw.written.Bytes()) {
				t.Fatalf("writes=%d closes=%d wire=%x", raw.writes, raw.closes, raw.written.Bytes())
			}
		}
	}
}
