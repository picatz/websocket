//go:build !js

package browser_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// TestNode runs this package's js/wasm tests in Node.js against a native Go
// server. Node 22 and later provide the same WebSocket object as browsers. The
// test is skipped when node is missing or too old.
func TestNode(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a js/wasm test binary")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	if out, err := exec.Command(node, "-e", "process.exit(typeof WebSocket === 'function' ? 0 : 1)").CombinedOutput(); err != nil {
		t.Skipf("node has no global WebSocket (Node 22 or later required): %v %s", err, out)
	}

	srv := httptest.NewServer(testServer())
	defer srv.Close()

	bin := filepath.Join(t.TempDir(), "browser.test.wasm")
	build := exec.CommandContext(t.Context(), filepath.Join(runtime.GOROOT(), "bin", "go"), "test", "-c", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build js/wasm tests: %v\n%s", err, out)
	}

	args := []string{filepath.Join(runtime.GOROOT(), "lib", "wasm", "wasm_exec_node.js"), bin, "-test.count=1"}
	if testing.Verbose() {
		args = append(args, "-test.v")
	}
	run := exec.CommandContext(t.Context(), node, args...)
	// wasm_exec copies the environment into the module's limited memory, so
	// pass only what the tests read.
	run.Env = []string{"BROWSER_TEST_URL=ws" + strings.TrimPrefix(srv.URL, "http")}
	out, err := run.CombinedOutput()
	t.Logf("js/wasm tests:\n%s", out)
	if err != nil {
		t.Fatalf("js/wasm tests failed: %v", err)
	}
}

// testServer serves the endpoints the js/wasm tests dial.
func testServer() http.Handler {
	anyOrigin := &websocket.Upgrader{CheckOrigin: func(*http.Request, string) bool { return true }}
	mux := http.NewServeMux()

	// /echo returns every message unchanged.
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		conn, err := anyOrigin.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, data, err := conn.ReadMessage(context.Background())
			if err != nil {
				return
			}
			if err := conn.WriteMessage(context.Background(), messageType, data); err != nil {
				return
			}
		}
	})

	// /subprotocol selects "chat" and reports it back.
	mux.HandleFunc("/subprotocol", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: anyOrigin.CheckOrigin, Subprotocols: []string{"chat"}}).Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("chat"))
		_, _, _ = conn.ReadMessage(context.Background())
	})

	// /goodbye sends two messages and closes with StatusGoingAway.
	mux.HandleFunc("/goodbye", func(w http.ResponseWriter, r *http.Request) {
		conn, err := anyOrigin.Upgrade(w, r)
		if err != nil {
			return
		}
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("one"))
		_ = conn.WriteMessage(context.Background(), websocket.BinaryMessage, []byte("two"))
		_ = conn.CloseWithStatus(websocket.StatusGoingAway, "bye")
		// Wait for the client's reply so the close is clean.
		_, _, _ = conn.ReadMessage(context.Background())
	})

	// /closestatus records the client's close code and reason, which
	// /lastclose then sends back as "<code> <reason>".
	var lastClose = make(chan string, 1)
	mux.HandleFunc("/closestatus", func(w http.ResponseWriter, r *http.Request) {
		conn, err := anyOrigin.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage(context.Background())
			if ce, ok := errors.AsType[*websocket.CloseError](err); ok {
				lastClose <- fmt.Sprintf("%d %s", int(ce.Code), ce.Reason)
			}
			if err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/lastclose", func(w http.ResponseWriter, r *http.Request) {
		conn, err := anyOrigin.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		select {
		case got := <-lastClose:
			_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(got))
		case <-time.After(5 * time.Second):
			_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("no close received"))
		}
		_, _, _ = conn.ReadMessage(context.Background())
	})

	// /stall upgrades and never reads, until the client goes away.
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) {
		conn, err := anyOrigin.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})

	// /reject fails the handshake.
	mux.HandleFunc("/reject", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	})

	// /silent upgrades and then only reads, so a client read never completes.
	mux.HandleFunc("/silent", func(w http.ResponseWriter, r *http.Request) {
		conn, err := anyOrigin.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(context.Background()); err != nil {
				return
			}
		}
	})
	return mux
}
