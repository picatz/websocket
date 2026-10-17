package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBrowserExample builds the client, checks the page and its assets are
// served, and runs the client in Node.js against the /echo endpoint. Node 22
// and later provide the same WebSocket object as browsers.
func TestBrowserExample(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a js/wasm binary")
	}
	a, err := buildClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.handler())
	defer srv.Close()

	for path, contentType := range map[string]string{
		"/":             "text/html; charset=utf-8",
		"/wasm_exec.js": "text/javascript; charset=utf-8",
		"/main.wasm":    "application/wasm",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(body) == 0 {
			t.Errorf("GET %s = %d with %d bytes; want 200 with a body", path, resp.StatusCode, len(body))
		}
		if got := resp.Header.Get("Content-Type"); got != contentType {
			t.Errorf("GET %s Content-Type = %q; want %q", path, got, contentType)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	if out, err := exec.Command(node, "-e", "process.exit(typeof WebSocket === 'function' ? 0 : 1)").CombinedOutput(); err != nil {
		t.Skipf("node has no global WebSocket (Node 22 or later required): %v %s", err, out)
	}

	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	wasm := filepath.Join(t.TempDir(), "main.wasm")
	if err := os.WriteFile(wasm, a.wasm, 0o644); err != nil {
		t.Fatal(err)
	}
	run := exec.CommandContext(t.Context(), node,
		filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm", "wasm_exec_node.js"),
		wasm, "hello", "wasm")
	// wasm_exec copies the environment into the module's limited memory, so
	// pass only what the client reads.
	run.Env = []string{"ECHO_URL=ws" + strings.TrimPrefix(srv.URL, "http") + "/echo"}
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("client: %v\n%s", err, out)
	}
	if got, want := string(out), "< hello\n< wasm\n"; got != want {
		t.Errorf("client output = %q; want %q", got, want)
	}
}
