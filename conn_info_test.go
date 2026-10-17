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
				tls   *tls.ConnectionState
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
				serverInfo <- info{r.TLS, conn.LocalAddr().String()}
				if got := conn.RemoteAddr().String(); got != r.RemoteAddr {
					t.Errorf("server RemoteAddr = %s, want %s", got, r.RemoteAddr)
				}
				_, _, _ = conn.ReadMessage(context.Background())
			})
			var srv *httptest.Server
			var d Dialer
			url := ""
			if secure {
				srv = httptest.NewTLSServer(handler)
				d.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
				url = "wss" + srv.URL[5:]
			} else {
				srv = httptest.NewServer(handler)
				url = "ws" + srv.URL[4:]
			}
			defer srv.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, resp, err := d.Dial(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			// The client reads TLS state from Dial's response, the server
			// from the request, as with net/http.
			if state := resp.TLS; (state != nil) != secure || (secure && (!state.HandshakeComplete || state.NegotiatedProtocol != "http/1.1")) {
				t.Fatalf("client resp.TLS = %+v", state)
			}
			got := <-serverInfo
			if (got.tls != nil) != secure || (secure && !got.tls.HandshakeComplete) {
				t.Fatalf("server r.TLS = %+v", got.tls)
			}
			if conn.RemoteAddr().String() != got.local || conn.RemoteAddr().String() != srv.Listener.Addr().String() {
				t.Fatalf("client RemoteAddr = %s, server LocalAddr = %s", conn.RemoteAddr(), got.local)
			}
		})
	}
}
