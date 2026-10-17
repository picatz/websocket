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

// subprotocolServer upgrades with u and reports the request's offer and the
// server's negotiated subprotocol.
func subprotocolServer(t *testing.T, u *Upgrader) (url string, offers <-chan []string, selected <-chan string) {
	t.Helper()
	offerc, selectedc := make(chan []string, 1), make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offerc <- r.Header.Values("Sec-WebSocket-Protocol")
		conn, err := u.Upgrade(w, r)
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

func dialSubprotocol(t *testing.T, url string, d *Dialer) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, _, err := d.Dial(ctx, url)
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
			url, offers, selected := subprotocolServer(t, &Upgrader{Subprotocols: tt.server})
			conn := dialSubprotocol(t, url, &Dialer{Subprotocols: tt.client})
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
	url, _, selected := subprotocolServer(t, &Upgrader{Header: http.Header{"Sec-WebSocket-Protocol": {"chat"}}})
	conn := dialSubprotocol(t, url, &Dialer{Header: http.Header{"Sec-WebSocket-Protocol": {"superchat, chat"}}})
	if got := <-selected; got != "chat" {
		t.Errorf("server Subprotocol = %q", got)
	}
	if got := conn.Subprotocol(); got != "chat" {
		t.Errorf("client Subprotocol = %q", got)
	}
}

// Dial reads its Dialer without modifying it or the values it refers to.
func TestDialerIsNotModified(t *testing.T) {
	d := &Dialer{Subprotocols: []string{"a", "b"}, Header: http.Header{"X-Trace": {"1"}}}
	for range 2 {
		url, offers, _ := subprotocolServer(t, &Upgrader{})
		dialSubprotocol(t, url, d)
		if got := <-offers; !reflect.DeepEqual(got, []string{"a, b"}) {
			t.Errorf("offer = %q", got)
		}
	}
	if !reflect.DeepEqual(d, &Dialer{Subprotocols: []string{"a", "b"}, Header: http.Header{"X-Trace": {"1"}}}) {
		t.Errorf("Dialer modified: %+v", d)
	}
}

func TestDialRejectsInvalidSubprotocolOption(t *testing.T) {
	for name, d := range map[string]*Dialer{
		"empty token":     {Subprotocols: []string{"a", ""}},
		"comma":           {Subprotocols: []string{"a,b"}},
		"space":           {Subprotocols: []string{"a b"}},
		"duplicate":       {Subprotocols: []string{"a", "a"}},
		"header conflict": {Subprotocols: []string{"a"}, Header: http.Header{"sec-websocket-protocol": {"b"}}},
	} {
		t.Run(name, func(t *testing.T) {
			dialed := false
			d.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, errors.New("unexpected dial")
			}
			conn, resp, err := d.Dial(t.Context(), "ws://example.test/")
			if conn != nil || resp != nil || !errors.Is(err, errInvalidHandshakeHeader) || !errors.Is(err, errInvalidSubprotocol) {
				t.Fatalf("Dial = %v, %v, %v", conn, resp, err)
			}
			if dialed {
				t.Fatal("dialed with an invalid subprotocol option")
			}
		})
	}
}

func TestUpgradeRejectsInvalidSubprotocolOption(t *testing.T) {
	for name, u := range map[string]*Upgrader{
		"empty token":     {Subprotocols: []string{""}},
		"duplicate":       {Subprotocols: []string{"chat", "chat"}},
		"slash":           {Subprotocols: []string{"chat/1"}},
		"header conflict": {Subprotocols: []string{"chat"}, Header: http.Header{"Sec-WebSocket-Protocol": {"chat"}}},
	} {
		t.Run(name, func(t *testing.T) {
			r := upgradeRequest()
			r.Header.Set("Sec-WebSocket-Protocol", "chat")
			w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
			conn, err := u.Upgrade(w, r)
			if conn != nil || !errors.Is(err, errInvalidHandshakeHeader) || !errors.Is(err, errInvalidSubprotocol) {
				t.Fatalf("Upgrade = %v, %v", conn, err)
			}
			if w.calls != 0 {
				t.Fatal("hijacked with an invalid subprotocol option")
			}
		})
	}
}

func TestNewConnSubprotocol(t *testing.T) {
	if got := frameConn(nil, false, 0).Subprotocol(); got != "" {
		t.Fatalf("Subprotocol = %q", got)
	}
	c := newTestConn(&memoryConn{}, ConnConfig{Subprotocol: "chat"})
	if got := c.Subprotocol(); got != "chat" {
		t.Fatalf("Subprotocol = %q, want the configured one", got)
	}
}
