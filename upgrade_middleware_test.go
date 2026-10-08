package websocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A typical status-recording middleware can expose its underlying writer
// without itself implementing all of net/http's optional interfaces.
type middlewareResponseWriter struct {
	http.ResponseWriter
	unwraps int
	status  int
	writes  int
}

func (w *middlewareResponseWriter) Unwrap() http.ResponseWriter {
	w.unwraps++
	return w.ResponseWriter
}

func (w *middlewareResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *middlewareResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.ResponseWriter.Write(p)
}

type middlewareHijacker struct {
	*middlewareResponseWriter
	hijack func() (net.Conn, *bufio.ReadWriter, error)
	calls  int
}

func (w *middlewareHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	return w.hijack()
}

func TestUpgradeMiddlewareLoopback(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner := &middlewareResponseWriter{ResponseWriter: w}
		outer := &middlewareResponseWriter{ResponseWriter: inner}
		conn, err := Upgrade(outer, r, WithResponseHeader(http.Header{"X-Websocket-Test": {"wrapped"}}))
		if err != nil {
			done <- err
			http.Error(w, "upgrade failed", http.StatusInternalServerError)
			return
		}
		defer conn.conn.Close()
		if inner.unwraps != 1 || outer.unwraps != 1 || inner.status != 0 || outer.status != 0 || inner.writes != 0 || outer.writes != 0 {
			done <- fmt.Errorf("unexpected middleware use: inner=%+v outer=%+v", inner, outer)
			return
		}
		if err := conn.conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			done <- err
			return
		}
		opcode, data, err := conn.ReadMessage()
		if err != nil || opcode != TextMessage || string(data) != "first" {
			done <- fmt.Errorf("pipelined message = %d, %q, %v", opcode, data, err)
			return
		}
		done <- conn.WriteMessage(TextMessage, data)
	}))
	defer srv.Close()
	peer, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Send the first masked frame in the same write as the handshake so it may
	// already be buffered by net/http when the handler hijacks the connection.
	request := "GET / HTTP/1.1\r\nHost: " + strings.TrimPrefix(srv.URL, "http://") + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	wire := append([]byte(request), []byte{0x81, 0x85, 0, 0, 0, 0, 'f', 'i', 'r', 's', 't'}...)
	if n, err := peer.Write(wire); err != nil || n != len(wire) {
		t.Fatalf("write request: %d/%d, %v", n, len(wire), err)
	}
	reader := bufio.NewReader(peer)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("X-Websocket-Test") != "wrapped" {
		t.Fatalf("response = %s, %v", resp.Status, resp.Header)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != computeAcceptKey("dGhlIHNhbXBsZSBub25jZQ==") {
		t.Fatal("invalid handshake accept")
	}
	client := NewConn(peer, false, nil)
	client.rw.Reader = reader
	if opcode, data, err := client.ReadMessage(); err != nil || opcode != TextMessage || string(data) != "first" {
		t.Fatalf("echo = %d, %q, %v", opcode, data, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeMiddlewarePreservesHijackedBuffers(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	wire := []byte{0x81, 0x81, 0, 0, 0, 0, 'x'}
	rw := bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(wire)), bufio.NewWriter(raw))
	if _, err := rw.Peek(len(wire)); err != nil {
		t.Fatal(err)
	}
	base := &hijackResponse{httptest.NewRecorder(), raw, rw}
	inner := &middlewareResponseWriter{ResponseWriter: base}
	outer := &middlewareResponseWriter{ResponseWriter: inner}
	conn, err := Upgrade(outer, upgradeRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.conn.Close()
	if conn.conn != raw || conn.rw != rw || rw.Reader.Buffered() != len(wire) || rw.Writer.Buffered() != 0 {
		t.Fatal("Upgrade replaced the hijacked connection or buffers, consumed input, or did not flush the handshake")
	}
	if opcode, data, err := conn.ReadMessage(); err != nil || opcode != TextMessage || string(data) != "x" {
		t.Fatalf("buffered message = %d, %q, %v", opcode, data, err)
	}
	if !strings.HasPrefix(raw.written.String(), "HTTP/1.1 101 Switching Protocols\r\n") {
		t.Fatalf("wire response = %q", raw.written.String())
	}
}

func TestUpgradeMiddlewareHijackerTakesPriority(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%v", wrap), func(t *testing.T) {
			raw := &memoryConn{Reader: bytes.NewReader(nil)}
			rw := bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))
			inner := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
			writer := &middlewareHijacker{
				middlewareResponseWriter: &middlewareResponseWriter{ResponseWriter: inner},
				hijack:                   func() (net.Conn, *bufio.ReadWriter, error) { return raw, rw, nil },
			}
			var w http.ResponseWriter = writer
			if wrap {
				w = &middlewareResponseWriter{ResponseWriter: w}
			}
			conn, err := Upgrade(w, upgradeRequest())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.conn.Close()
			if writer.calls != 1 || writer.unwraps != 0 || inner.calls != 0 || conn.rw != rw {
				t.Fatalf("Hijack calls = %d, Unwrap calls = %d, inner Hijack calls = %d", writer.calls, writer.unwraps, inner.calls)
			}
		})
	}
}

func TestUpgradeMiddlewareUnsupported(t *testing.T) {
	for _, depth := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			var w http.ResponseWriter = recorder
			for range depth {
				w = &middlewareResponseWriter{ResponseWriter: w}
			}
			conn, err := Upgrade(w, upgradeRequest())
			if conn != nil || err != ErrNotHijacker {
				t.Fatalf("Upgrade = %v, %v; want ErrNotHijacker", conn, err)
			}
			if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 || len(recorder.Header()) != 0 || recorder.Flushed {
				t.Fatal("unsupported writer received a response")
			}
		})
	}
}

type middlewareWriteFailureConn struct {
	*memoryConn
	closes int
}

func (c *middlewareWriteFailureConn) Write([]byte) (int, error) {
	return 0, errors.New("handshake transport write failed")
}

func (c *middlewareWriteFailureConn) Close() error {
	c.closes++
	return nil
}

func TestUpgradeMiddlewareHandshakeWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		bufferSize int
		operation  string
	}{{1, "write"}, {4096, "flush"}} {
		t.Run(tc.operation, func(t *testing.T) {
			raw := &middlewareWriteFailureConn{memoryConn: &memoryConn{Reader: bytes.NewReader(nil)}}
			rw := bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriterSize(raw, tc.bufferSize))
			base := &hijackResponse{httptest.NewRecorder(), raw, rw}
			outer := &middlewareResponseWriter{ResponseWriter: base}
			conn, err := Upgrade(outer, upgradeRequest())
			if conn != nil || !errors.Is(err, ErrHandshakeFailed) || !strings.Contains(err.Error(), "failed to "+tc.operation+" handshake response") {
				t.Fatalf("Upgrade = %v, %v", conn, err)
			}
			if raw.closes != 1 || raw.written.Len() != 0 || outer.writes != 0 || outer.status != 0 || base.Body.Len() != 0 {
				t.Fatalf("failed handshake did not close transport or used ResponseWriter: closes=%d, middleware=%+v", raw.closes, outer)
			}
		})
	}
}

func TestUpgradeMiddlewareHijackErrors(t *testing.T) {
	for _, cause := range []error{errors.New("hijack failed"), context.Canceled, http.ErrNotSupported, fmt.Errorf("wrapped: %w", http.ErrNotSupported)} {
		for _, wrap := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%v", cause, wrap), func(t *testing.T) {
				recorder := httptest.NewRecorder()
				writer := &middlewareHijacker{
					middlewareResponseWriter: &middlewareResponseWriter{ResponseWriter: recorder},
					hijack:                   func() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, cause },
				}
				var w http.ResponseWriter = writer
				if wrap {
					w = &middlewareResponseWriter{ResponseWriter: w}
				}
				conn, err := Upgrade(w, upgradeRequest())
				if conn != nil {
					t.Fatal("unexpected connection after hijack failure")
				}
				if wrap && errors.Is(cause, http.ErrNotSupported) {
					if err != ErrNotHijacker {
						t.Fatalf("error = %v; want ErrNotHijacker", err)
					}
				} else if !errors.Is(err, ErrHandshakeFailed) || err.Error() != ErrHandshakeFailed.Error()+": "+cause.Error() {
					t.Fatalf("error = %v; want original ErrHandshakeFailed contract", err)
				}
				if writer.calls != 1 || writer.unwraps != 0 || writer.status != 0 || writer.writes != 0 || recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
					t.Fatalf("failure wrote a response or bypassed Hijack: %+v", writer)
				}
			})
		}
	}
}

func TestUpgradeMiddlewareValidatesBeforeUnwrap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		opts   []UpgradeOption
		want   error
	}{
		{"connection", func(r *http.Request) { r.Header.Del("Connection") }, nil, ErrInvalidConnectionHeader},
		{"upgrade", func(r *http.Request) { r.Header.Del("Upgrade") }, nil, ErrInvalidUpgradeHeader},
		{"method", func(r *http.Request) { r.Method = http.MethodPost }, nil, ErrInvalidMethod},
		{"version", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Version", "13") }, nil, ErrUnsupportedVersion},
		{"key", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "invalid") }, nil, ErrInvalidSecKey},
		{"protocol", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Protocol", "bad token") }, nil, ErrInvalidSubprotocol},
		{"extensions", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Extensions", "invalid;") }, nil, ErrInvalidExtension},
		{"custom response", nil, []UpgradeOption{WithResponseHeader(http.Header{"X-Test": {"bad\r\nheader"}})}, ErrInvalidHandshakeHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := upgradeRequest()
			if tc.change != nil {
				tc.change(r)
			}
			inner := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
			outer := &middlewareResponseWriter{ResponseWriter: inner}
			conn, err := Upgrade(outer, r, tc.opts...)
			if conn != nil || !errors.Is(err, tc.want) {
				t.Fatalf("Upgrade = %v, %v; want %v", conn, err, tc.want)
			}
			if outer.unwraps != 0 || inner.calls != 0 || outer.status != 0 || outer.writes != 0 || inner.Body.Len() != 0 {
				t.Fatalf("invalid handshake reached middleware: %+v", outer)
			}
		})
	}
}
