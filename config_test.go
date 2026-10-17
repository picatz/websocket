package websocket

import (
	"compress/flate"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
		if got := NewConn(&memoryConn{}, ConnConfig{MaxMessageSize: tc.in}).maxBytes; got != tc.want {
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
			if conn != nil || resp != nil || !errors.Is(err, errInvalidConfig) {
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
			if conn != nil || !errors.Is(err, errInvalidConfig) || errors.Is(err, ErrBadHandshake) {
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

// A shared Upgrader gives every connection its own extensions: a fresh
// permessage-deflate instance and a fresh call to NewExtensions.
func TestUpgraderBuildsExtensionsPerConnection(t *testing.T) {
	var calls atomic.Int32
	u := &Upgrader{
		Compression: &Compression{},
		NewExtensions: func() []Extension {
			calls.Add(1)
			return nil
		},
	}
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
	if got := calls.Load(); got != 2 {
		t.Fatalf("NewExtensions called %d times, want 2", got)
	}
}

// Compression is negotiated before the NewExtensions extensions.
func TestBuildExtensionsOrder(t *testing.T) {
	custom := &xorExtension{}
	exts, err := buildExtensions(&Compression{}, func() []Extension { return []Extension{custom} })
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) != 2 || exts[1] != Extension(custom) {
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
