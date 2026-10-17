// Command browser serves a Go WebAssembly page that talks to a Go WebSocket
// server.
//
// The page runs the program in the client directory, built with GOOS=js and
// GOARCH=wasm. It dials the server with the
// [github.com/picatz/websocket/browser] package, which wraps the browser's
// own WebSocket object, and the server answers with [websocket.Upgrade]. Both
// halves use the same message types and close errors.
//
// Run it with the Go toolchain on PATH, then open the printed address:
//
//	go run ./examples/browser
//
// The server builds the client when it starts, so there is nothing to build
// first.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/picatz/websocket"
)

//go:embed index.html
var indexHTML []byte

// assets are the files the page loads besides index.html.
type assets struct {
	// wasm is the client compiled with GOOS=js GOARCH=wasm.
	wasm []byte
	// wasmExec is the wasm_exec.js glue from the same Go toolchain.
	wasmExec []byte
}

// buildClient compiles the client package with the Go toolchain on PATH and
// returns it together with that toolchain's wasm_exec.js. The two must come
// from the same Go release.
func buildClient(ctx context.Context) (assets, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return assets{}, fmt.Errorf("find go toolchain: %w", err)
	}
	goroot, err := exec.CommandContext(ctx, goBin, "env", "GOROOT").Output()
	if err != nil {
		return assets{}, fmt.Errorf("go env GOROOT: %w", err)
	}
	wasmExec, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm", "wasm_exec.js"))
	if err != nil {
		return assets{}, fmt.Errorf("read wasm_exec.js: %w", err)
	}

	dir, err := os.MkdirTemp("", "browser-example")
	if err != nil {
		return assets{}, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "main.wasm")
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, "github.com/picatz/websocket/examples/browser/client")
	build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	if msg, err := build.CombinedOutput(); err != nil {
		return assets{}, fmt.Errorf("build client: %w\n%s", err, msg)
	}
	wasm, err := os.ReadFile(out)
	if err != nil {
		return assets{}, err
	}
	return assets{wasm: wasm, wasmExec: wasmExec}, nil
}

// handler serves the page, its assets, and the /echo WebSocket endpoint.
func (a assets) handler() http.Handler {
	mux := http.NewServeMux()
	serve := func(contentType string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		}
	}
	mux.Handle("GET /{$}", serve("text/html; charset=utf-8", indexHTML))
	mux.Handle("GET /wasm_exec.js", serve("text/javascript; charset=utf-8", a.wasmExec))
	// WebAssembly.instantiateStreaming requires this content type.
	mux.Handle("GET /main.wasm", serve("application/wasm", a.wasm))
	mux.HandleFunc("GET /echo", echo)
	return mux
}

// echo returns every message unchanged. The page is served from the same
// origin, so Upgrade's default same-origin check accepts it.
func echo(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Upgrade(w, r)
	if err != nil {
		upgradeError(w, err)
		return
	}
	defer conn.Close()
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		err = conn.WriteMessageContext(ctx, messageType, message)
		cancel()
		if err != nil {
			return
		}
	}
}

func upgradeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, websocket.ErrOriginNotAllowed):
		http.Error(w, "WebSocket origin not allowed", http.StatusForbidden)
	case errors.Is(err, websocket.ErrUnsupportedVersion):
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "Unsupported WebSocket version", http.StatusUpgradeRequired)
	case !errors.Is(err, websocket.ErrHandshakeFailed):
		// A failed hijack or 101 write may already own the transport.
		http.Error(w, "Invalid WebSocket handshake", http.StatusBadRequest)
	}
}

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	log.Print("building the js/wasm client")
	a, err := buildClient(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("open http://%s in a browser", *addr)
	log.Fatal(http.ListenAndServe(*addr, a.handler()))
}
