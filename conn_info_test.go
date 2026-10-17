package websocket

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConnInfo(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "ws", true: "wss"}[secure], func(t *testing.T) {
			type info struct {
				state tls.ConnectionState
				ok    bool
				local string
			}
			serverInfo := make(chan info, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := Upgrade(w, r)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				state, ok := conn.ConnectionState()
				serverInfo <- info{state, ok, conn.LocalAddr().String()}
				if got := conn.RemoteAddr().String(); got != r.RemoteAddr {
					t.Errorf("server RemoteAddr = %s, want %s", got, r.RemoteAddr)
				}
				_, _, _ = conn.ReadMessage()
			})
			var srv *httptest.Server
			var options []DialOption
			url := ""
			if secure {
				srv = httptest.NewTLSServer(handler)
				options = append(options, WithTLSConfig(srv.Client().Transport.(*http.Transport).TLSClientConfig))
				url = "wss" + srv.URL[5:]
			} else {
				srv = httptest.NewServer(handler)
				url = "ws" + srv.URL[4:]
			}
			defer srv.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, _, err := Dial(ctx, url, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			state, ok := conn.ConnectionState()
			if ok != secure || (secure && (!state.HandshakeComplete || state.NegotiatedProtocol != "http/1.1")) {
				t.Fatalf("client ConnectionState = %+v, %v", state, ok)
			}
			got := <-serverInfo
			if got.ok != secure || (secure && !got.state.HandshakeComplete) {
				t.Fatalf("server ConnectionState = %+v, %v", got.state, got.ok)
			}
			if conn.RemoteAddr().String() != got.local || conn.RemoteAddr().String() != srv.Listener.Addr().String() {
				t.Fatalf("client RemoteAddr = %s, server LocalAddr = %s", conn.RemoteAddr(), got.local)
			}
		})
	}
}
