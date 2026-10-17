// Package benchmarks compares picatz/websocket with other Go WebSocket
// libraries on identical workloads over loopback TCP. It is a separate module
// so the core module keeps no third-party dependencies. See BENCHMARKS.md in
// the repository root for how to run it and how to read the results.
package benchmarks

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

var sizes = []int{16, 1 << 10, 16 << 10, 256 << 10}

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

func dial(tb testing.TB, lib library, srv *httptest.Server) client {
	tb.Helper()
	c, err := lib.dial(tb.Context(), wsURL(srv))
	if err != nil {
		tb.Fatalf("%s: dial: %v", lib.name, err)
	}
	tb.Cleanup(func() { c.close() })
	return c
}

// TestEcho checks every adapter round-trips each benchmark payload, so a
// broken adapter fails loudly instead of producing a fast, wrong number.
func TestEcho(t *testing.T) {
	for _, lib := range libraries {
		t.Run(lib.name, func(t *testing.T) {
			srv := httptest.NewServer(lib.echo)
			defer srv.Close()
			c := dial(t, lib, srv)
			for _, size := range sizes {
				p := payload(size)
				if err := c.write(p); err != nil {
					t.Fatalf("write %d: %v", size, err)
				}
				got, err := c.read()
				if err != nil || !bytes.Equal(got, p) {
					t.Fatalf("read %d: got %d bytes, %v", size, len(got), err)
				}
			}
		})
	}
}

func payload(size int) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(i)
	}
	return p
}

// BenchmarkEcho measures one client write and one echoed read of a binary
// message over a single loopback connection. Both endpoints use the library
// under test, so time and allocations cover both sides of the round trip.
func BenchmarkEcho(b *testing.B) {
	for _, size := range sizes {
		for _, lib := range libraries {
			b.Run(fmt.Sprintf("size=%d/lib=%s", size, lib.name), func(b *testing.B) {
				srv := httptest.NewServer(lib.echo)
				defer srv.Close()
				c := dial(b, lib, srv)
				p := payload(size)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for b.Loop() {
					if err := c.write(p); err != nil {
						b.Fatal(err)
					}
					if _, err := c.read(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkEchoParallel is BenchmarkEcho with one connection per parallel
// worker, all served by one server, to show contention and scheduler costs.
func BenchmarkEchoParallel(b *testing.B) {
	const size = 1 << 10
	for _, lib := range libraries {
		b.Run(fmt.Sprintf("size=%d/lib=%s", size, lib.name), func(b *testing.B) {
			srv := httptest.NewServer(lib.echo)
			defer srv.Close()
			p := payload(size)
			b.SetBytes(size)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				c, err := lib.dial(b.Context(), wsURL(srv))
				if err != nil {
					b.Error(err)
					return
				}
				defer c.close()
				for pb.Next() {
					if err := c.write(p); err != nil {
						b.Error(err)
						return
					}
					if _, err := c.read(); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// BenchmarkHandshake measures opening and closing one connection: TCP
// connect, the HTTP upgrade on both sides, and close.
func BenchmarkHandshake(b *testing.B) {
	for _, lib := range libraries {
		b.Run("lib="+lib.name, func(b *testing.B) {
			srv := httptest.NewServer(lib.echo)
			defer srv.Close()
			b.ReportAllocs()
			for b.Loop() {
				c, err := lib.dial(b.Context(), wsURL(srv))
				if err != nil {
					b.Fatal(err)
				}
				c.close()
			}
		})
	}
}

// idleConns is how many connections BenchmarkIdleConn holds open at once.
const idleConns = 500

// BenchmarkIdleConn measures the server-side memory each idle connection
// keeps: heap and goroutine stacks, after a GC, divided by idleConns. Clients
// are raw TCP connections that send only a handshake, so no client library
// memory is counted. The baseline row hijacks the request and blocks in a
// read while keeping net/http's buffered reader and writer, as a plain
// goroutine-per-connection server would. Libraries that reuse or drop those
// buffers can come in below it. Each round opens idleConns connections, so run
// it with -benchtime=1x and a -count of several rounds.
func BenchmarkIdleConn(b *testing.B) {
	type server struct {
		name    string
		handler http.HandlerFunc
	}
	servers := []server{{"baseline", hijackAndWait}}
	for _, lib := range libraries {
		servers = append(servers, server{lib.name, lib.echo})
	}
	for _, s := range servers {
		b.Run("lib="+s.name, func(b *testing.B) {
			srv := httptest.NewServer(s.handler)
			defer srv.Close()
			var bytesPerConn, goroutinesPerConn float64
			var rounds int
			for b.Loop() {
				before := memInUse()
				goroutines := runtime.NumGoroutine()
				conns := make([]net.Conn, 0, idleConns)
				for range idleConns {
					conns = append(conns, rawHandshake(b, srv.Listener.Addr().String()))
				}
				// Let each handler reach its blocking read.
				time.Sleep(50 * time.Millisecond)
				after := memInUse()
				bytesPerConn += float64(after-before) / idleConns
				goroutinesPerConn += float64(runtime.NumGoroutine()-goroutines) / idleConns
				rounds++
				for _, c := range conns {
					c.Close()
				}
				srv.CloseClientConnections()
			}
			b.ReportMetric(0, "ns/op")
			b.ReportMetric(bytesPerConn/float64(rounds), "B/conn")
			b.ReportMetric(goroutinesPerConn/float64(rounds), "goroutines/conn")
		})
	}
}

// memInUse returns heap and stack bytes in use after a full collection.
func memInUse() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse + m.StackInuse)
}

// hijackAndWait takes over the connection and blocks reading it, holding the
// same goroutine and buffered reader a WebSocket server would.
func hijackAndWait(w http.ResponseWriter, r *http.Request) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	brw.Flush()
	var p [1]byte
	conn.Read(p[:])
}

// rawHandshake opens a TCP connection, sends an RFC 6455 opening handshake and
// waits for the 101 response. The fixed key is valid; the client does not
// check Sec-WebSocket-Accept, since only the server is being measured.
func rawHandshake(tb testing.TB, addr string) net.Conn {
	tb.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		tb.Fatal(err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		tb.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		tb.Fatalf("handshake status %s", resp.Status)
	}
	return conn
}
