package benchmarks

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// broadcastClients is how many connections BenchmarkBroadcast sends to.
const broadcastClients = 100

// BenchmarkBroadcast measures a server sending one 1 KiB binary message to
// broadcastClients connections in turn, as a chat or pub/sub hub does, and
// every client reading it. One op is one message delivered to every client,
// so time and allocations cover the server's writes and all clients' reads,
// and MB/s counts the bytes delivered to all clients.
func BenchmarkBroadcast(b *testing.B) {
	const size = 1 << 10
	for _, lib := range libraries {
		b.Run(fmt.Sprintf("size=%d/clients=%d/lib=%s", size, broadcastClients, lib.name), func(b *testing.B) {
			conns, received := broadcastPair(b, lib)
			p := payload(size)
			b.SetBytes(size * broadcastClients)
			b.ReportAllocs()
			gc := startGCCost()
			for b.Loop() {
				for _, c := range conns {
					if err := c.write(p); err != nil {
						b.Fatal(err)
					}
				}
				for range conns {
					if err := <-received; err != nil {
						b.Fatal(err)
					}
				}
			}
			gc.report(b, b.N)
		})
	}
}

// TestBroadcast checks every adapter delivers server-sent messages.
func TestBroadcast(t *testing.T) {
	for _, lib := range libraries {
		t.Run(lib.name, func(t *testing.T) {
			conns, received := broadcastPair(t, lib)
			for _, c := range conns {
				if err := c.write(payload(1 << 10)); err != nil {
					t.Fatal(err)
				}
			}
			for range conns {
				if err := <-received; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// broadcastPair connects broadcastClients clients and returns the server side
// of each connection. Each client reads in its own goroutine and sends nil
// on received for every message, or the error that stopped it.
func broadcastPair(tb testing.TB, lib library) ([]client, <-chan error) {
	tb.Helper()
	accepted := make(chan client)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := lib.accept(w, r)
		if err != nil {
			return
		}
		accepted <- c
		<-done // Keep the handler, and so the connection, alive.
	}))
	received := make(chan error, broadcastClients)
	var readers sync.WaitGroup
	tb.Cleanup(func() {
		close(done)
		srv.Close()
		readers.Wait()
	})
	conns := make([]client, 0, broadcastClients)
	for range broadcastClients {
		c := dial(tb, lib, srv)
		conns = append(conns, <-accepted)
		readers.Go(func() {
			for {
				if _, err := c.read(); err != nil {
					return // The connection closed at cleanup.
				}
				received <- nil
			}
		})
	}
	tb.Cleanup(func() {
		for _, c := range conns {
			c.close()
		}
	})
	return conns, received
}
