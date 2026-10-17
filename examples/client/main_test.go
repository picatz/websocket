package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/picatz/websocket"
)

// echoServer echoes text and answers "bytes" with a binary message.
func echoServer(t *testing.T, upgrader *websocket.Upgrader) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "deny" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch string(data) {
			case "bytes":
				typ, data = websocket.BinaryMessage, []byte{0xca, 0xfe}
			case "subprotocol":
				data = []byte(conn.Subprotocol())
			case "kick":
				conn.CloseWithStatus(websocket.StatusPolicyViolation, "kicked")
				return
			}
			if err := conn.WriteMessage(typ, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestRun(t *testing.T) {
	url := echoServer(t, &websocket.Upgrader{Subprotocols: []string{"v2"}})
	for _, tc := range []struct {
		name       string
		args       []string
		stdin      string
		wantOut    string
		wantStderr string
		wantErr    bool
	}{
		{name: "echo", args: []string{url}, stdin: "hello\nworld\n", wantOut: "hello\nworld\n", wantStderr: "closed: 1000"},
		{name: "binary", args: []string{url}, stdin: "bytes\n", wantOut: "ca fe\n"},
		{name: "subprotocol", args: []string{"-subprotocol", "v1,v2", url}, stdin: "subprotocol\n", wantOut: "v2\n", wantStderr: "subprotocol: v2"},
		{name: "compress", args: []string{"-compress", url}, stdin: "hello\n", wantOut: "hello\n"},
		{name: "server close", args: []string{url}, stdin: "kick\n", wantStderr: "closed: 1008 kicked", wantErr: true},
		{name: "rejected", args: []string{"-H", "Authorization: deny", url}, wantErr: true},
		{name: "no url", wantErr: true, wantStderr: "usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("run = %v, want error %v (stderr %q)", err, tc.wantErr, stderr.String())
			}
			if got := stdout.String(); got != tc.wantOut {
				t.Errorf("stdout = %q, want %q", got, tc.wantOut)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// TestRunInterrupted checks that ending the context while standard input is
// still open closes the connection with StatusGoingAway.
func TestRunInterrupted(t *testing.T) {
	url := echoServer(t, &websocket.Upgrader{})
	ctx, cancel := context.WithCancel(t.Context())
	stdin, input := io.Pipe()
	defer input.Close()
	echoed := make(chan string, 1)

	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		done <- run(ctx, []string{url}, stdin, writerFunc(func(p []byte) { echoed <- string(p) }), &stderr)
	}()
	io.WriteString(input, "hello\n")
	if got := <-echoed; got != "hello\n" {
		t.Fatalf("stdout = %q", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
	if !strings.Contains(stderr.String(), "closed: 1001") {
		t.Fatalf("stderr = %q, want a 1001 close", stderr.String())
	}
}

// writerFunc is an io.Writer that hands each write to a function.
type writerFunc func(p []byte)

func (f writerFunc) Write(p []byte) (int, error) {
	f(bytes.Clone(p))
	return len(p), nil
}
