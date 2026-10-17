package websocket

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeConns returns a connected server and client over a synchronous pipe.
func pipeConns(t *testing.T) (server, client *Conn) {
	t.Helper()
	a, b := net.Pipe()
	server, client = NewConn(a, ConnConfig{Server: true}), NewConn(b, ConnConfig{})
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return server, client
}

func TestNetConnStreamsAcrossMessages(t *testing.T) {
	server, client := pipeConns(t)
	sn := NetConn(t.Context(), server, BinaryMessage)
	cn := NetConn(t.Context(), client, BinaryMessage)

	go func() {
		for _, chunk := range []string{"hel", "lo, ", "", "tunnel"} {
			if _, err := cn.Write([]byte(chunk)); err != nil {
				t.Error(err)
				return
			}
		}
		_ = cn.Close()
	}()

	// Small reads span message boundaries; the peer's close ends the stream
	// with the bare io.EOF that io.ReadAll and io.Copy expect.
	var got bytes.Buffer
	buf := make([]byte, 3)
	for {
		n, err := sn.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if got.String() != "hello, tunnel" {
		t.Fatalf("stream = %q", got.String())
	}
	if n, err := sn.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("read after EOF = %d, %v", n, err)
	}
}

// HTTP/1.1 over the tunnel exercises the adapter the way real stream
// protocols use a net.Conn: buffered reads, concurrent I/O and Close.
func TestNetConnCarriesHTTP(t *testing.T) {
	conns := make(chan net.Conn, 1)
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		conns <- NetConn(context.Background(), conn, BinaryMessage)
	}))
	defer wsSrv.Close()

	ln := &chanListener{conns: conns, done: make(chan struct{})}
	inner := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunnelled "+r.URL.Path)
	})}
	go func() { _ = inner.Serve(ln) }()
	defer inner.Close()

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, _, err := Dial(ctx, "ws"+wsSrv.URL[4:])
		if err != nil {
			return nil, err
		}
		return NetConn(context.Background(), conn, BinaryMessage), nil
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, path := range []string{"/a", "/b"} { // the second request reuses the tunnel
		resp, err := client.Get("http://tunnel.invalid" + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != "tunnelled "+path {
			t.Fatalf("GET %s = %q, %v", path, body, err)
		}
	}
}

type chanListener struct {
	conns <-chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestNetConnRejectsOtherMessageType(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader([]byte{0x81, 1, 'x'})}
	nc := NetConn(t.Context(), NewConn(raw, ConnConfig{}), BinaryMessage)
	if _, err := nc.Read(make([]byte, 8)); !errors.Is(err, ErrUnexpectedFrame) {
		t.Fatalf("Read = %v, want ErrUnexpectedFrame", err)
	}
	if got := failureClosePayload(t, raw.written.Bytes(), false); !bytes.Equal(got, []byte{0x03, 0xeb}) {
		t.Fatalf("close payload = %x, want StatusUnsupportedData", got)
	}
	if _, err := nc.Read(make([]byte, 8)); !errors.Is(err, ErrUnexpectedFrame) {
		t.Fatalf("second Read = %v, want the sticky error", err)
	}
}

func TestNetConnTextMessages(t *testing.T) {
	server, client := pipeConns(t)
	cn := NetConn(t.Context(), client, TextMessage)
	sn := NetConn(t.Context(), server, TextMessage)
	if _, err := cn.Write([]byte{0xff}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("invalid UTF-8 write = %v", err)
	}
	go func() { _, _ = cn.Write([]byte("ünïcode")) }()
	buf := make([]byte, len("ünïcode"))
	if _, err := io.ReadFull(sn, buf); err != nil || string(buf) != "ünïcode" {
		t.Fatalf("read = %q, %v", buf, err)
	}
}

func TestNetConnContextAbortsBlockedRead(t *testing.T) {
	_, client := pipeConns(t)
	ctx, cancel := context.WithCancel(t.Context())
	nc := NetConn(ctx, client, BinaryMessage)
	errc := make(chan error, 1)
	go func() {
		_, err := nc.Read(make([]byte, 1))
		errc <- err
	}()
	cancel()
	select {
	case err := <-errc:
		if err == nil || err == io.EOF {
			t.Fatalf("Read after cancel = %v, want a failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not return after the context ended")
	}
}

func TestNetConnCloseMatchesNetErrClosed(t *testing.T) {
	server, client := pipeConns(t)
	go func() { _, _, _ = server.ReadMessage() }() // Accept the close frame.
	nc := NetConn(t.Context(), client, BinaryMessage)
	_ = nc.Close()
	if _, err := nc.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read after Close = %v", err)
	}
	if _, err := nc.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
}

func TestNetConnReadDeadlineIsTimeout(t *testing.T) {
	_, client := pipeConns(t)
	nc := NetConn(t.Context(), client, BinaryMessage)
	if err := nc.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := nc.Read(make([]byte, 1))
	if ne, ok := errors.AsType[net.Error](err); !ok || !ne.Timeout() {
		t.Fatalf("Read past deadline = %v, want a timeout net.Error", err)
	}
}

func TestNetConnZeroLengthRead(t *testing.T) {
	_, client := pipeConns(t)
	nc := NetConn(t.Context(), client, BinaryMessage)
	if n, err := nc.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v", n, err)
	}
}

func TestNetConnAddrs(t *testing.T) {
	_, client := pipeConns(t)
	nc := NetConn(t.Context(), client, BinaryMessage)
	if nc.LocalAddr() != client.conn.LocalAddr() || nc.RemoteAddr() != client.conn.RemoteAddr() {
		t.Fatal("addresses do not come from the transport")
	}
}

func TestNetConnPanicsOnControlType(t *testing.T) {
	for _, typ := range []Opcode{ContinuationFrame, CloseMessage, PingMessage, PongMessage} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(r.(string), "NetConn") {
					t.Errorf("%v: recover = %v", typ, r)
				}
			}()
			NetConn(t.Context(), frameConn(nil, false, 0), typ)
		}()
	}
}

// io.Copy compares with == io.EOF, so a peer close must end it without error.
func TestNetConnPeerCloseEndsIOCopy(t *testing.T) {
	server, client := pipeConns(t)
	sn := NetConn(t.Context(), server, BinaryMessage)
	go func() {
		_ = client.WriteMessage(BinaryMessage, []byte("payload"))
		_ = client.CloseWithStatus(StatusGoingAway, "done")
	}()
	var got bytes.Buffer
	if n, err := io.Copy(&got, sn); err != nil || n != 7 || got.String() != "payload" {
		t.Fatalf("io.Copy = %d, %v (%q)", n, err, got.String())
	}
}

// An abnormal close must not read as a clean end of stream.
func TestNetConnAbnormalCloseFailsIOCopy(t *testing.T) {
	server, client := pipeConns(t)
	sn := NetConn(t.Context(), server, BinaryMessage)
	go func() {
		_ = client.WriteMessage(BinaryMessage, []byte("partial"))
		_ = client.CloseWithStatus(StatusInternalServerError, "crashed")
	}()
	var got bytes.Buffer
	n, err := io.Copy(&got, sn)
	if n != 7 || CloseStatus(err) != StatusInternalServerError || !errors.Is(err, io.EOF) {
		t.Fatalf("io.Copy = %d, %v; want 7 bytes and a 1011 CloseError", n, err)
	}
	if _, err := sn.Read(make([]byte, 1)); CloseStatus(err) != StatusInternalServerError {
		t.Fatalf("Read after abnormal close = %v", err)
	}
}
