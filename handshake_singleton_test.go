package websocket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// RFC 6455 sections 11.3.3 and 11.3.5 forbid repeated Accept response fields
// and Version request fields, even when all copies have the same value.
func TestUpgradeRequiresSingleVersion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		valid  bool
	}{
		{"single", []string{"13"}, true},
		{"absent", nil, false},
		{"empty", []string{""}, false},
		{"unsupported", []string{"12"}, false},
		{"duplicate", []string{"13", "13"}, false},
		{"supported first", []string{"13", "12"}, false},
		{"supported last", []string{"12", "13"}, false},
		{"empty second", []string{"13", ""}, false},
		{"comma list", []string{"13, 13"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := upgradeRequest()
			r.Header.Del("Sec-WebSocket-Version")
			for _, value := range tc.values {
				r.Header.Add("Sec-WebSocket-Version", value)
			}
			w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
			conn, err := Upgrade(w, r)
			if conn != nil {
				t.Fatal("unexpected connection from failing hijacker")
			}
			if tc.valid {
				if w.calls != 1 || !errors.Is(err, ErrHandshakeFailed) {
					t.Fatalf("valid version: error = %v, hijacks = %d", err, w.calls)
				}
			} else if !errors.Is(err, ErrUnsupportedVersion) || w.calls != 0 || w.Body.Len() != 0 {
				t.Fatalf("invalid version: error = %v, hijacks = %d, body = %q", err, w.calls, w.Body.String())
			}
		})
	}
}

func TestDialRequiresSingleAccept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string // "$accept" stands for the request's computed accept key.
		valid  bool
	}{
		{"single", []string{"$accept"}, true},
		{"whitespace", []string{" \t$accept \t"}, true},
		{"absent", nil, false},
		{"empty", []string{""}, false},
		{"wrong", []string{"wrong"}, false},
		{"duplicate", []string{"$accept", "$accept"}, false},
		{"correct first", []string{"$accept", "wrong"}, false},
		{"correct last", []string{"wrong", "$accept"}, false},
		{"empty second", []string{"$accept", ""}, false},
		{"comma list", []string{"$accept, $accept"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peerClosed := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					peerClosed <- err
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(2 * time.Second))
				fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: keep-alive\r\nConnection: Upgrade\r\nUpgrade: WebSocket\r\n")
				accept := computeAcceptKey(r.Header.Get("Sec-WebSocket-Key"))
				for i, value := range tc.values {
					name := "Sec-WebSocket-Accept"
					if i%2 == 1 {
						name = "sec-websocket-accept" // HTTP field names are case-insensitive.
					}
					fmt.Fprintf(rw, "%s: %s\r\n", name, strings.ReplaceAll(value, "$accept", accept))
				}
				fmt.Fprint(rw, "\r\n")
				// Valid handshakes must preserve a first frame buffered with the response.
				if tc.valid {
					rw.Write([]byte{0x82, 1, 'x'})
				}
				if err := rw.Flush(); err != nil {
					peerClosed <- err
					return
				}
				var b [1]byte
				_, err = rw.Read(b[:])
				peerClosed <- err
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			conn, resp, err := Dial(ctx, "ws"+srv.URL[4:])
			if conn != nil {
				defer conn.conn.Close()
			}
			if resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("response = %v, error = %v", resp, err)
			}
			if tc.valid {
				if err != nil || conn == nil {
					t.Fatalf("valid accept: connection = %v, error = %v", conn, err)
				}
				opcode, payload, err := conn.ReadMessage()
				if err != nil || opcode != BinaryMessage || string(payload) != "x" {
					t.Fatalf("buffered message = (%v, %q, %v)", opcode, payload, err)
				}
				conn.conn.Close()
			} else if conn != nil || !errors.Is(err, ErrInvalidSecAccept) {
				t.Fatalf("invalid accept: connection = %v, error = %v", conn, err)
			}
			if err := <-peerClosed; !errors.Is(err, io.EOF) {
				t.Fatalf("transport was not closed cleanly: %v", err)
			}
		})
	}
}
