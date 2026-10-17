package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// listen returns a TCP listener on a free local port, closed with the test.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// target starts a TCP service that handles each connection with handle.
func target(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln := listen(t)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				handle(conn)
			}()
		}
	}()
	return ln.Addr().String()
}

// tunnel starts a tunnel server forwarding to addr and a client in front of
// it, and returns the client's local address.
func tunnel(t *testing.T, addr string) string {
	t.Helper()
	srv := httptest.NewServer(server(addr))
	t.Cleanup(srv.Close)
	ln := listen(t)
	done := make(chan error, 1)
	go func() { done <- client(t.Context(), ln, "ws"+strings.TrimPrefix(srv.URL, "http")) }()
	t.Cleanup(func() {
		ln.Close()
		<-done
	})
	return ln.Addr().String()
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn
}

func TestTunnel(t *testing.T) {
	upper := target(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			io.WriteString(conn, strings.ToUpper(line))
		}
	})
	local := tunnel(t, upper)

	// Two connections at once, each with its own WebSocket.
	a, b := dial(t, local), dial(t, local)
	ra, rb := bufio.NewReader(a), bufio.NewReader(b)
	for i, line := range []string{"hello", "tunnel", strings.Repeat("x", 100_000)} {
		conn, r := a, ra
		if i%2 == 1 {
			conn, r = b, rb
		}
		io.WriteString(conn, line+"\n")
		got, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.ToUpper(line) + "\n"; got != want {
			t.Fatalf("got %d bytes, want %d", len(got), len(want))
		}
	}
}

// TestTargetCloses checks that the end of the target's stream reaches the
// local connection as EOF.
func TestTargetCloses(t *testing.T) {
	greet := target(t, func(conn net.Conn) { io.WriteString(conn, "bye\n") })
	conn := dial(t, tunnel(t, greet))
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "bye\n" {
		t.Fatalf("ReadAll = %q, %v; want \"bye\\n\" and EOF", got, err)
	}
}

// TestTargetDown checks that a target that refuses connections fails the
// handshake with 502 and closes the local connection.
func TestTargetDown(t *testing.T) {
	ln := listen(t)
	down := ln.Addr().String()
	ln.Close()

	rec := httptest.NewRecorder()
	server(down)(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	conn := dial(t, tunnel(t, down))
	if n, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("Read = %d, %v; want EOF", n, err)
	}
}
