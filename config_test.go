package websocket

import (
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMessageLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, DefaultMaxMessageSize},
		{-1, 0},
		{-1 << 30, 0},
		{1, 1},
		{64 << 10, 64 << 10},
	} {
		if got := messageLimit(tc.in); got != tc.want {
			t.Errorf("messageLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want int64 }{
		{0, DefaultMaxResponseHeaderBytes},
		{-1, 0},
		{4096, 4096},
	} {
		if got := headLimit(tc.in); got != tc.want {
			t.Errorf("headLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestNewConnConfigLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, DefaultMaxMessageSize},
		{-1, 0},
		{512, 512},
	} {
		if got := newTestConn(&memoryConn{}, ConnConfig{MaxMessageSize: tc.in}).maxBytes; got != tc.want {
			t.Errorf("ConnConfig{MaxMessageSize: %d} limit = %d, want %d", tc.in, got, tc.want)
		}
	}
}

var invalidCompressions = map[string]Compression{
	"level below HuffmanOnly":    {Level: -3},
	"level above Best":           {Level: 10},
	"negative threshold":         {Threshold: -1},
	"client window below 8":      {ClientMaxWindowBits: 7},
	"client window above 15":     {ClientMaxWindowBits: 16},
	"server window below 8":      {ServerMaxWindowBits: 1},
	"server window negative":     {ServerMaxWindowBits: -1},
	"client window far too high": {ClientMaxWindowBits: 1 << 20},
}

// An invalid Compression fails Dial before any connection is made.
func TestDialRejectsInvalidCompression(t *testing.T) {
	for name, c := range invalidCompressions {
		t.Run(name, func(t *testing.T) {
			d := &Dialer{Compression: &c, NetDialContext: func(context.Context, string, string) (net.Conn, error) {
				t.Error("dialed with an invalid Compression")
				return nil, errors.New("unexpected dial")
			}}
			conn, resp, err := d.Dial(t.Context(), "ws://example.test/")
			if conn != nil || resp != nil || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Dial = %v, %v, %v", conn, resp, err)
			}
		})
	}
}

// An invalid Compression is the server's fault: 500, before hijacking.
func TestUpgradeRejectsInvalidCompression(t *testing.T) {
	for name, c := range invalidCompressions {
		t.Run(name, func(t *testing.T) {
			r := upgradeRequest()
			r.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate")
			w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
			conn, err := (&Upgrader{Compression: &c}).Upgrade(w, r)
			if conn != nil || !errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrBadHandshake) {
				t.Fatalf("Upgrade = %v, %v", conn, err)
			}
			if w.calls != 0 || w.Code != http.StatusInternalServerError {
				t.Fatalf("hijacks = %d, status = %d; want 0 and 500", w.calls, w.Code)
			}
		})
	}
}

func TestCompressionExtension(t *testing.T) {
	type params = pmdParameters
	for name, tc := range map[string]struct {
		c                       Compression
		level, threshold, limit int
		configured              params
	}{
		"zero":               {Compression{}, flate.DefaultCompression, 0, DefaultCompressionMemoryLimit, params{}},
		"level":              {Compression{Level: flate.BestCompression}, flate.BestCompression, 0, DefaultCompressionMemoryLimit, params{}},
		"huffman only":       {Compression{Level: flate.HuffmanOnly}, flate.HuffmanOnly, 0, DefaultCompressionMemoryLimit, params{}},
		"threshold":          {Compression{Threshold: 256}, flate.DefaultCompression, 256, DefaultCompressionMemoryLimit, params{}},
		"memory limit":       {Compression{MemoryLimit: 8 << 10}, flate.DefaultCompression, 0, 8 << 10, params{}},
		"unlimited memory":   {Compression{MemoryLimit: -1}, flate.DefaultCompression, 0, 0, params{}},
		"client no takeover": {Compression{ClientNoContextTakeover: true}, flate.DefaultCompression, 0, DefaultCompressionMemoryLimit, params{clientNoContextTakeover: true}},
		"server no takeover": {Compression{ServerNoContextTakeover: true}, flate.DefaultCompression, 0, DefaultCompressionMemoryLimit, params{serverNoContextTakeover: true}},
		"windows":            {Compression{ClientMaxWindowBits: 9, ServerMaxWindowBits: 15}, flate.DefaultCompression, 0, DefaultCompressionMemoryLimit, params{clientMaxWindowBits: 9, serverMaxWindowBits: 15}},
	} {
		t.Run(name, func(t *testing.T) {
			ext, err := tc.c.extension()
			if err != nil {
				t.Fatal(err)
			}
			pmd := ext.(*perMessageDeflate)
			if pmd.level != tc.level || pmd.threshold != tc.threshold || pmd.memoryLimit != tc.limit || pmd.configured != tc.configured {
				t.Fatalf("level %d threshold %d limit %d configured %+v; want %d %d %d %+v",
					pmd.level, pmd.threshold, pmd.memoryLimit, pmd.configured, tc.level, tc.threshold, tc.limit, tc.configured)
			}
		})
	}
}

// A shared Upgrader gives every connection a fresh permessage-deflate
// instance.
func TestUpgraderBuildsExtensionsPerConnection(t *testing.T) {
	u := &Upgrader{Compression: &Compression{}}
	conns := make(chan *Conn, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := u.Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		conns <- c
	}))
	defer srv.Close()

	d := &Dialer{Compression: &Compression{}}
	for range 2 {
		c, resp, err := d.Dial(t.Context(), "ws"+srv.URL[4:])
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get("Sec-WebSocket-Extensions"); got == "" {
			t.Fatal("compression not negotiated")
		}
		defer c.Close()
	}
	a, b := <-conns, <-conns
	defer a.Close()
	defer b.Close()
	if len(a.extensions) != 1 || len(b.extensions) != 1 || a.extensions[0] == b.extensions[0] {
		t.Fatalf("extensions %v and %v, want one distinct instance each", a.extensions, b.extensions)
	}
}

// Compression is negotiated before any other extension.
func TestBuildExtensionsOrder(t *testing.T) {
	custom := &xorExtension{}
	exts, err := buildExtensions(&Compression{}, []frameExtension{custom})
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) != 2 || exts[1] != frameExtension(custom) {
		t.Fatalf("extensions = %v", exts)
	}
	if _, ok := exts[0].(*perMessageDeflate); !ok {
		t.Fatalf("first extension = %T, want permessage-deflate", exts[0])
	}
	if exts, err := buildExtensions(nil, nil); err != nil || exts != nil {
		t.Fatalf("buildExtensions(nil, nil) = %v, %v", exts, err)
	}
}

// A nil *Dialer and *Upgrader behave as their zero values.
func TestNilConfigIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var u *Upgrader
		c, err := u.Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		c.Close()
	}))
	defer srv.Close()
	var d *Dialer
	c, _, err := d.Dial(t.Context(), "ws"+srv.URL[4:])
	if err != nil {
		t.Fatal(err)
	}
	if c.maxBytes != DefaultMaxMessageSize {
		t.Fatalf("limit = %d", c.maxBytes)
	}
	c.Close()
}

// Every caller misconfiguration matches ErrInvalidConfig, from Dial before
// connecting and from Upgrade with a 500.
func TestMisconfigurationMatchesErrInvalidConfig(t *testing.T) {
	noDial := func(context.Context, string, string) (net.Conn, error) {
		t.Error("dialed with an invalid Dialer")
		return nil, errors.New("unexpected dial")
	}
	for name, d := range map[string]*Dialer{
		"subprotocol":     {Subprotocols: []string{"a b"}},
		"header conflict": {Subprotocols: []string{"a"}, Header: http.Header{"Sec-WebSocket-Protocol": {"a"}}},
		"reserved header": {Header: http.Header{"Sec-WebSocket-Key": {"x"}}},
		"compression":     {Compression: &Compression{Level: 42}},
	} {
		t.Run("Dial/"+name, func(t *testing.T) {
			d.NetDialContext = noDial
			if _, _, err := d.Dial(t.Context(), "ws://example.test/"); !errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrBadHandshake) {
				t.Fatalf("Dial error = %v, want ErrInvalidConfig", err)
			}
		})
	}
	for name, u := range map[string]*Upgrader{
		"subprotocol":     {Subprotocols: []string{"a b"}},
		"header conflict": {Subprotocols: []string{"chat"}, Header: http.Header{"Sec-WebSocket-Protocol": {"chat"}}},
		"reserved header": {Header: http.Header{"Upgrade": {"h2c"}}},
		"compression":     {Compression: &Compression{Threshold: -1}},
	} {
		t.Run("Upgrade/"+name, func(t *testing.T) {
			r := upgradeRequest()
			r.Header.Set("Sec-WebSocket-Protocol", "chat")
			w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
			if _, err := u.Upgrade(w, r); !errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrBadHandshake) {
				t.Fatalf("Upgrade error = %v, want ErrInvalidConfig", err)
			}
			if w.calls != 0 || w.Code != http.StatusInternalServerError {
				t.Fatalf("hijacks = %d, status = %d; want 0 and 500", w.calls, w.Code)
			}
		})
	}
}

// NewConn takes the agreed permessage-deflate parameters from the response
// value, in both roles, and the two ends then exchange compressed messages.
func TestNewConnAgreedCompression(t *testing.T) {
	const agreement = "permessage-deflate; server_no_context_takeover; client_max_window_bits=10"
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	server, err := NewConn(a, ConnConfig{Server: true, Extensions: agreement})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewConn(b, ConnConfig{Extensions: agreement, Compression: &Compression{Level: 9, ClientMaxWindowBits: 15}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Conn{server, client} {
		pmd, ok := c.extensions[0].(*perMessageDeflate)
		if len(c.extensions) != 1 || !ok || !pmd.enabled || pmd.server != c.isServer {
			t.Fatalf("extensions = %v", c.extensions)
		}
		want := pmdParameters{serverNoContextTakeover: true, clientMaxWindowBits: 10}
		if pmd.negotiated != want {
			t.Fatalf("negotiated %+v, want %+v", pmd.negotiated, want)
		}
	}
	if got := client.extensions[0].(*perMessageDeflate); got.level != 9 || got.localWindowBits() != 10 {
		t.Fatalf("client level %d window %d; want 9 and the agreed 10", got.level, got.localWindowBits())
	}

	msg := bytes.Repeat([]byte("agreed "), 200)
	errc := make(chan error, 1)
	go func() { errc <- client.WriteMessage(context.Background(), TextMessage, msg) }()
	_, got, err := server.ReadMessage(context.Background())
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("server read %d bytes, %v", len(got), err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestNewConnRejectsInvalidAgreement(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  ConnConfig
		want error
	}{
		"unknown extension":   {ConnConfig{Extensions: "x-webkit-deflate-frame"}, errInvalidExtension},
		"two extensions":      {ConnConfig{Extensions: "permessage-deflate, permessage-deflate"}, errInvalidExtension},
		"syntax":              {ConnConfig{Extensions: "permessage-deflate;"}, errInvalidExtension},
		"bare window":         {ConnConfig{Extensions: "permessage-deflate; client_max_window_bits"}, errInvalidExtension},
		"window out of range": {ConnConfig{Extensions: "permessage-deflate; server_max_window_bits=16"}, errInvalidExtension},
		"unknown parameter":   {ConnConfig{Extensions: "permessage-deflate; x=1"}, errInvalidExtension},
		"invalid compression": {ConnConfig{Extensions: "permessage-deflate", Compression: &Compression{Level: 11}}, ErrInvalidConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if c, err := NewConn(&memoryConn{}, tc.cfg); c != nil || !errors.Is(err, tc.want) || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewConn = %v, %v; want %v and ErrInvalidConfig", c, err, tc.want)
			}
		})
	}
	// Compression alone, without an agreement, enables nothing.
	c, err := NewConn(&memoryConn{}, ConnConfig{Compression: &Compression{}})
	if err != nil || len(c.extensions) != 0 {
		t.Fatalf("NewConn = %v, %v", c.extensions, err)
	}
}
