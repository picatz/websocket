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
	"testing"
	"time"
)

type hijackResponse struct {
	*httptest.ResponseRecorder
	conn net.Conn
	rw   *bufio.ReadWriter
}

func (w *hijackResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.conn, w.rw, nil }

func upgradeRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/ws", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return r
}

func TestUpgradePreservesBufferedFrame(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	wire := []byte{0x81, 0x81, 0, 0, 0, 0, 'x'}
	rw := bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(wire)), bufio.NewWriter(raw))
	w := &hijackResponse{httptest.NewRecorder(), raw, rw}
	conn, err := Upgrade(w, upgradeRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil || string(data) != "x" {
		t.Fatalf("message = %q, %v; want x", data, err)
	}
}

func TestDialPreservesBufferedFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer raw.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		rw.Write([]byte{0x81, 1, 'x'})
		rw.Flush()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, _, err := Dial(ctx, "ws"+srv.URL[4:])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, data, err := conn.ReadMessage()
	if err != nil || string(data) != "x" {
		t.Fatalf("message = %q, %v; want x", data, err)
	}
}

func TestDialHonorsTLSConfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
	}))
	defer srv.Close()
	config := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	conn, _, err := Dial(t.Context(), "wss"+srv.URL[5:], WithTLSConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if config.ServerName != "" || len(config.NextProtos) != 0 {
		t.Fatal("Dial mutated caller TLS configuration")
	}
}

func TestDialContextCoversHandshake(t *testing.T) {
	accepted := make(chan net.Conn, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, _, err := Dial(ctx, "ws://"+listener.Addr().String())
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("not accepted")
	}
	defer peer.Close()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		peer.Close()
		<-result
		t.Fatal("Dial ignored cancellation during handshake")
	}
}

func TestUpgradeRejectsInvalidKey(t *testing.T) {
	for _, key := range []string{"!", "eA==", "dGhlIHNhbXBsZSBub25jZQ", "dGhlIHNhbXBsZSBub25jZQ==, dGhlIHNhbXBsZSBub25jZQ=="} {
		t.Run(key, func(t *testing.T) {
			r := upgradeRequest()
			r.Header.Set("Sec-WebSocket-Key", key)
			_, err := Upgrade(httptest.NewRecorder(), r)
			if !errors.Is(err, ErrInvalidSecKey) {
				t.Fatalf("error = %v, want invalid key", err)
			}
		})
	}
	r := upgradeRequest()
	r.Header.Add("Sec-WebSocket-Key", r.Header.Get("Sec-WebSocket-Key"))
	if _, err := Upgrade(httptest.NewRecorder(), r); !errors.Is(err, ErrInvalidSecKey) {
		t.Fatalf("duplicate key error = %v", err)
	}
}

func TestUpgradeMaxMessageSize(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader([]byte{0x82, 0x85, 0, 0, 0, 0, 1, 2, 3, 4, 5})}
	w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
	conn, err := Upgrade(w, upgradeRequest(), WithUpgradeMaxMessageSize(4))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("error = %v, want ErrPayloadTooLarge", err)
	}
}

func TestDialRejectsInvalidURL(t *testing.T) {
	for _, url := range []string{"http://example.test", "ws:///no-host", "ws://user@example.test", "ws://example.test/#fragment"} {
		t.Run(url, func(t *testing.T) {
			if _, _, err := Dial(t.Context(), url); !errors.Is(err, ErrBadHandshake) {
				t.Fatalf("error = %v, want ErrBadHandshake", err)
			}
		})
	}
}

func TestDialContextDetachedAfterHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		opcode, data, err := conn.ReadMessage()
		if err == nil {
			conn.WriteMessage(opcode, data)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn, _, err := Dial(ctx, "ws"+srv.URL[4:])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	if err := conn.WriteMessage(TextMessage, []byte("still alive")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "still alive" {
		t.Fatalf("message = %q, %v", data, err)
	}
}
