package websocket_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

func TestDialRejectedBodyIsReadable(t *testing.T) {
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"content length": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "sign in first", http.StatusUnauthorized)
			},
			want: "sign in first\n",
		},
		"chunked": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, "not ")
				w.(http.Flusher).Flush()
				io.WriteString(w, "allowed")
			},
			want: "not allowed",
		},
		"truncated at 4 KiB": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, strings.Repeat("x", 10<<10))
			},
			want: strings.Repeat("x", 4<<10),
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, resp, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"))
			if !errors.Is(err, websocket.ErrBadHandshake) || resp == nil {
				t.Fatalf("Dial: resp = %v, err = %v; want a response and ErrBadHandshake", resp, err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != tc.want {
				t.Fatalf("body = %q (%d bytes), %v; want %q", body[:min(len(body), 64)], len(body), err, tc.want[:min(len(tc.want), 64)])
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A server that sends a rejection head and then stalls the body cannot hold
// Dial, even when Dial's context has no deadline.
func TestDialRejectedBodyStallIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = http.ReadRequest(bufio.NewReader(c))
		io.WriteString(c, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 100\r\n\r\npartial")
		time.Sleep(5 * time.Second)
	}()
	start := time.Now()
	_, resp, err := websocket.Dial(context.Background(), "ws://"+ln.Addr().String())
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Dial took %v", elapsed)
	}
	if !errors.Is(err, websocket.ErrBadHandshake) || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Dial: resp = %v, err = %v", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "partial" {
		t.Fatalf("body = %q, want the part that arrived", body)
	}
}
