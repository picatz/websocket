package websocket

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// subprotocolServer upgrades with options and reports the request's offer and
// the server's negotiated subprotocol.
func subprotocolServer(t *testing.T, options ...UpgradeOption) (url string, offers <-chan []string, selected <-chan string) {
	t.Helper()
	offerc, selectedc := make(chan []string, 1), make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offerc <- r.Header.Values("Sec-WebSocket-Protocol")
		conn, err := Upgrade(w, r, options...)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		selectedc <- conn.Subprotocol()
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[4:], offerc, selectedc
}

func dialSubprotocol(t *testing.T, url string, options ...DialOption) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, _, err := Dial(ctx, url, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestSubprotocolOptionsNegotiate(t *testing.T) {
	tests := []struct {
		name     string
		client   []string
		server   []string
		wantWire []string
		want     string
	}{
		// The server's preference order wins over the client's.
		{"server preference", []string{"v1.chat", "v2.chat"}, []string{"v2.chat", "v1.chat"}, []string{"v1.chat, v2.chat"}, "v2.chat"},
		{"single overlap", []string{"a", "b", "c"}, []string{"z", "c"}, []string{"a, b, c"}, "c"},
		// Selection is case-sensitive (RFC 6455 section 4.1).
		{"case-sensitive", []string{"Chat"}, []string{"chat"}, []string{"Chat"}, ""},
		{"no overlap", []string{"a"}, []string{"b"}, []string{"a"}, ""},
		{"client offers none", nil, []string{"a"}, nil, ""},
		{"server supports none", []string{"a"}, nil, []string{"a"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, offers, selected := subprotocolServer(t, WithUpgradeSubprotocols(tt.server...))
			conn := dialSubprotocol(t, url, WithSubprotocols(tt.client...))
			if got := <-offers; !reflect.DeepEqual(got, tt.wantWire) {
				t.Errorf("offer on the wire = %q, want %q", got, tt.wantWire)
			}
			if got := <-selected; got != tt.want {
				t.Errorf("server Subprotocol = %q, want %q", got, tt.want)
			}
			if got := conn.Subprotocol(); got != tt.want {
				t.Errorf("client Subprotocol = %q, want %q", got, tt.want)
			}
		})
	}
}

// Raw headers keep working, and Subprotocol reports what they negotiated.
func TestSubprotocolFromRawHeaders(t *testing.T) {
	url, _, selected := subprotocolServer(t, WithResponseHeader(http.Header{"Sec-WebSocket-Protocol": {"chat"}}))
	conn := dialSubprotocol(t, url, WithHeader(http.Header{"Sec-WebSocket-Protocol": {"superchat, chat"}}))
	if got := <-selected; got != "chat" {
		t.Errorf("server Subprotocol = %q", got)
	}
	if got := conn.Subprotocol(); got != "chat" {
		t.Errorf("client Subprotocol = %q", got)
	}
}

func TestSubprotocolOptionLastWins(t *testing.T) {
	url, offers, _ := subprotocolServer(t)
	dialSubprotocol(t, url, WithSubprotocols("a"), WithSubprotocols("b", "c"))
	if got := <-offers; !reflect.DeepEqual(got, []string{"b, c"}) {
		t.Errorf("offer = %q", got)
	}
	url, offers, _ = subprotocolServer(t)
	dialSubprotocol(t, url, WithSubprotocols("a"), WithSubprotocols())
	if got := <-offers; got != nil {
		t.Errorf("reset offer = %q, want none", got)
	}
}

func TestSubprotocolOptionDetachesCallerSlices(t *testing.T) {
	protocols := []string{"a"}
	option := WithSubprotocols(protocols...)
	protocols[0] = "not a token"
	header := http.Header{"X-Trace": {"1"}}
	url, offers, _ := subprotocolServer(t)
	dialSubprotocol(t, url, option, WithHeader(header))
	if got := <-offers; !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("offer = %q", got)
	}
	if !reflect.DeepEqual(header, http.Header{"X-Trace": {"1"}}) {
		t.Errorf("caller header modified: %v", header)
	}
}

func TestDialRejectsInvalidSubprotocolOption(t *testing.T) {
	for name, options := range map[string][]DialOption{
		"empty token":     {WithSubprotocols("a", "")},
		"comma":           {WithSubprotocols("a,b")},
		"space":           {WithSubprotocols("a b")},
		"duplicate":       {WithSubprotocols("a", "a")},
		"header conflict": {WithSubprotocols("a"), WithHeader(http.Header{"sec-websocket-protocol": {"b"}})},
	} {
		t.Run(name, func(t *testing.T) {
			dialed := false
			options = append(options, WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, errors.New("unexpected dial")
			}))
			conn, resp, err := Dial(t.Context(), "ws://example.test/", options...)
			if conn != nil || resp != nil || !errors.Is(err, ErrInvalidHandshakeHeader) || !errors.Is(err, ErrInvalidSubprotocol) {
				t.Fatalf("Dial = %v, %v, %v", conn, resp, err)
			}
			if dialed {
				t.Fatal("dialed with an invalid subprotocol option")
			}
		})
	}
}

func TestUpgradeRejectsInvalidSubprotocolOption(t *testing.T) {
	for name, options := range map[string][]UpgradeOption{
		"empty token":     {WithUpgradeSubprotocols("")},
		"duplicate":       {WithUpgradeSubprotocols("chat", "chat")},
		"slash":           {WithUpgradeSubprotocols("chat/1")},
		"header conflict": {WithUpgradeSubprotocols("chat"), WithResponseHeader(http.Header{"Sec-WebSocket-Protocol": {"chat"}})},
	} {
		t.Run(name, func(t *testing.T) {
			r := upgradeRequest()
			r.Header.Set("Sec-WebSocket-Protocol", "chat")
			w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
			conn, err := Upgrade(w, r, options...)
			if conn != nil || !errors.Is(err, ErrInvalidHandshakeHeader) || !errors.Is(err, ErrInvalidSubprotocol) {
				t.Fatalf("Upgrade = %v, %v", conn, err)
			}
			if w.calls != 0 {
				t.Fatal("hijacked with an invalid subprotocol option")
			}
		})
	}
}

func TestNewConnHasNoSubprotocol(t *testing.T) {
	if got := frameConn(nil, false, 0).Subprotocol(); got != "" {
		t.Fatalf("Subprotocol = %q", got)
	}
}
