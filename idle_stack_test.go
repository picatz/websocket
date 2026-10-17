package websocket

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestUpgradeKeepsInitialStack checks that an upgraded connection's handler
// goroutine keeps the runtime's initial stack. The handshake runs on that
// goroutine, and if it grows the stack past the initial size, the goroutine
// keeps the larger stack for as long as it waits for messages: the garbage
// collector only shrinks a stack that is mostly unused, and net/http's own
// frames below the handler use too much of it. On linux/amd64 that doubled
// the stack from 4 KiB to 8 KiB for every idle connection.
//
// The test runs itself again with GODEBUG=adaptivestackstart=0, so the
// initial size is the runtime's fixed minimum and the result does not depend
// on what other tests ran.
func TestUpgradeKeepsInitialStack(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes stack sizes")
	}
	const setting = "adaptivestackstart=0"
	if !strings.Contains(os.Getenv("GODEBUG"), setting) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestUpgradeKeepsInitialStack$", "-test.count=1")
		cmd.Env = append(os.Environ(), "GODEBUG="+setting)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	const conns = 200
	stackPerConn := func(handler http.HandlerFunc) float64 {
		srv := httptest.NewServer(handler)
		defer srv.Close()
		before := stackInUse()
		for range conns {
			c := idleHandshake(t, srv.Listener.Addr().String())
			defer c.Close()
		}
		// Each handler has answered its handshake before idleHandshake
		// returns, and then blocks in a read.
		return float64(stackInUse()-before) / conns
	}
	baseline := stackPerConn(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		brw.ReadByte()
	})
	got := stackPerConn(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		c.ReadMessage()
	})
	if got > baseline+512 {
		t.Fatalf("idle connection stack = %.0f bytes, want about the %.0f of a hijacked net/http connection", got, baseline)
	}
}

// stackInUse returns goroutine stack bytes in use after a full collection,
// which shrinks stacks that are mostly unused.
func stackInUse() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.StackInuse)
}

// idleHandshake opens a TCP connection, sends an opening handshake and waits
// for the 101 response.
func idleHandshake(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	return conn
}
