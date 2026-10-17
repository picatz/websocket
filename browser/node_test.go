//go:build !js

package browser_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	anyOrigin := websocket.WithUpgradeOriginCheck(func(*http.Request, string) bool { return true })
	mux := http.NewServeMux()

	// /echo returns every message unchanged.
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, anyOrigin)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(messageType, data); err != nil {
				return
			}
		}
	})

	// /subprotocol selects "chat" and reports it back.
	mux.HandleFunc("/subprotocol", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, anyOrigin, websocket.WithResponseHeader(http.Header{"Sec-WebSocket-Protocol": {"chat"}}))
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("chat"))
		_, _, _ = conn.ReadMessage()
	})

	// /goodbye sends two messages and closes normally.
	mux.HandleFunc("/goodbye", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, anyOrigin)
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte("one"))
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("two"))
		conn.Close()
	})

	// /reject fails the handshake.
	mux.HandleFunc("/reject", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	})

	// /silent upgrades and then only reads, so a client read never completes.
	mux.HandleFunc("/silent", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, anyOrigin)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	return mux
}
