package websocket

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Every ResponseWriter operation, including Header, is observable on rejection.
// originWriter records the response status and counts hijacks; a denied
// origin must produce exactly a 403 and never reach Hijack.
type originWriter struct {
	calls  int // Hijack calls
	status int
	header http.Header
}

func (w *originWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *originWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *originWriter) WriteHeader(status int)      { w.status = status }
func (w *originWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	return nil, nil, errors.New("unexpected hijack")
}

// originPolicy is an Upgrader with only CheckOrigin set.
func originPolicy(check func(*http.Request, string) bool) *Upgrader {
	return &Upgrader{CheckOrigin: check}
}

func originUpgrade(t *testing.T, r *http.Request, upgraders ...*Upgrader) (*Conn, error) {
	t.Helper()
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
	u := &Upgrader{}
	if len(upgraders) > 0 {
		u = upgraders[0]
	}
	c, err := u.Upgrade(w, r)
	if c != nil {
		t.Cleanup(func() { c.conn.Close() })
	}
	return c, err
}

func TestUpgradeDefaultOriginPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, host, origin string
		tls, allow         bool
	}{
		{"missing", "example.test", "<missing>", false, true},
		{"empty", "example.test", "", false, false},
		{"null", "example.test", "null", false, false},
		{"http", "example.test", "http://example.test", false, true},
		{"https", "example.test", "https://example.test", true, true},
		{"scheme", "example.test", "https://example.test", false, false},
		{"host", "example.test", "http://other.test", false, false},
		{"port", "example.test:8080", "http://example.test", false, false},
		{"http default port", "example.test:80", "http://example.test", false, true},
		{"https default port", "example.test", "https://example.test:443", true, true},
		{"numeric port", "example.test:0080", "HTTP://EXAMPLE.TEST:00080", false, true},
		{"case and OWS", "EXAMPLE.test", " \tHTTP://example.TEST\t ", false, true},
		{"nondefault port", "example.test:8080", "http://example.test:8080", false, true},
		{"IPv6", "[2001:db8::1]:80", "http://[2001:0DB8:0:0:0:0:0:1]", false, true},
		{"mapped IPv6", "[::ffff:7f00:1]", "http://[::ffff:127.0.0.1]", false, true},
		{"IPv4 distinct", "127.0.0.1", "http://[::ffff:127.0.0.1]", false, false},
		{"localhost alias", "127.0.0.1", "http://localhost", false, false},
		{"loopback alias", "[::1]", "http://127.0.0.1", false, false},
		{"trailing dot", "example.test.", "http://example.test", false, false},
		{"trailing dot exact", "example.test.", "http://EXAMPLE.TEST.", false, true},
		{"punycode", "xn--bcher-kva.test", "http://XN--BCHER-KVA.test", false, true},
		{"Unicode", "bücher.test", "http://bücher.test", false, false},
		{"unknown scheme", "example.test", "custom://example.test", false, false},
		{"invalid request host", "example.test/path", "http://example.test", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := upgradeRequest()
			r.Host = tc.host
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.origin != "<missing>" {
				r.Header.Set("Origin", tc.origin)
			}
			c, err := originUpgrade(t, r)
			if (err == nil) != tc.allow || (c != nil) != tc.allow || !tc.allow && !errors.Is(err, ErrOriginNotAllowed) {
				t.Fatalf("Upgrade = %v, %v; allow %v", c, err, tc.allow)
			}
		})
	}
}

func TestUpgradeRejectsAmbiguousOriginBeforeCallback(t *testing.T) {
	headers := []http.Header{
		{"Origin": {"http://example.test", "http://example.test"}},
		{"Origin": {"http://example.test", "http://evil.test"}},
		{"Origin": {"http://example.test"}, "origin": {"http://example.test"}},
		{"Origin": nil}, {"Origin": {}},
		{"Origin": {}, "origin": {"http://example.test"}},
	}
	for _, value := range []string{
		"", " \t", "NULL", "http://example.test,http://evil.test", "http://a,b.test", "http://example.test http://evil.test",
		"http://user@example.test", "http://example.test/", "http://example.test/path", "http://example.test?",
		"http://example.test?q=x", "http://example.test#", "http://example.test#fragment", "/relative", "http:example.test",
		"http://example.test\\evil", "http://ex%61mple.test", "http://[broken]", "http://[127.0.0.1]", "http://[::1%25eth0]",
		"http://::1", "http://[::1]extra", "http://example.test:", "http://example.test:+80", "http://example.test:abc",
		"http://example.test:65536", "http://example.test:80:90", "http://example.test\n", "http://exa\tmple.test",
		"http://example.test\x00", "http://example.test\x7f", "1http://example.test", "http://", "http:///example.test",
	} {
		headers = append(headers, http.Header{"Origin": {value}})
	}
	for _, header := range headers {
		t.Run(strings.ReplaceAll(header.Get("Origin"), "/", "_"), func(t *testing.T) {
			r := upgradeRequest()
			for k, v := range header {
				r.Header[k] = v
			}
			before := r.Header.Clone()
			w := new(originWriter)
			called := 0
			ext := &negotiationExtension{name: "example", offer: "example"}
			c, err := (&Upgrader{newExtensions: func() []frameExtension { return []frameExtension{ext} }, CheckOrigin: func(*http.Request, string) bool { called++; return true }}).Upgrade(w, r)
			if c != nil || err != ErrOriginNotAllowed || called != 0 || ext.calls != 0 || w.calls != 0 || w.status != http.StatusForbidden {
				t.Fatalf("conn %v, error %v, policy %d, extension %d, hijacks %d, status %d", c, err, called, ext.calls, w.calls, w.status)
			}
			if !reflect.DeepEqual(before, r.Header) {
				t.Fatal("mutated request headers")
			}
		})
	}
}

func TestUpgradeOriginOverride(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"<missing>", ""}, {"null", "null"}, {" \tHTTPS://PUBLIC.TEST:00443\t", "https://public.test"},
		{"CUSTOM+app://EXAMPLE.test:00080", "custom+app://example.test:80"},
		{"http://[2001:0DB8::1]:0080", "http://[2001:db8::1]"},
		{"http://[::ffff:192.0.2.1]", "http://[::ffff:c000:201]"},
		{"http://[::ffff:c000:201]", "http://[::ffff:c000:201]"},
		{"HTTPS://a!$&'()*+;=~B.test:443", "https://a!$&'()*+;=~b.test"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			r := upgradeRequest()
			if tc.value != "<missing>" {
				r.Header.Set("Origin", tc.value)
			}
			before := r.Clone(t.Context())
			calls := 0
			c, err := originUpgrade(t, r, originPolicy(func(got *http.Request, origin string) bool {
				calls++
				if got != r || origin != tc.want {
					t.Fatalf("callback = %p %q; want %p %q", got, origin, r, tc.want)
				}
				return true
			}))
			if err != nil || c == nil || calls != 1 {
				t.Fatalf("Upgrade = %v, %v; calls %d", c, err, calls)
			}
			if !reflect.DeepEqual(before.Header, r.Header) || !reflect.DeepEqual(before.URL, r.URL) {
				t.Fatal("request mutated")
			}
		})
	}
	allow := originPolicy(func(_ *http.Request, o string) bool { return o == "https://public.test" })
	deny := originPolicy(func(*http.Request, string) bool { return false })
	for _, tc := range []struct {
		name, origin string
		upgrader     *Upgrader
		allow        bool
	}{
		{"allow cross-origin", "https://public.test", allow, true},
		{"reject suffix", "https://public.test.evil", allow, false},
		{"replace default", "http://example.test", allow, false},
		{"deny missing", "<missing>", deny, false},
		{"deny same-origin", "http://example.test", deny, false},
		{"nil is same origin", "http://example.test", &Upgrader{}, true},
		{"nil allows missing", "<missing>", &Upgrader{}, true},
		{"nil rejects cross-origin", "https://public.test", &Upgrader{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := upgradeRequest()
			if tc.origin != "<missing>" {
				r.Header.Set("Origin", tc.origin)
			}
			_, err := originUpgrade(t, r, tc.upgrader)
			if (err == nil) != tc.allow {
				t.Fatalf("error %v; allow %v", err, tc.allow)
			}
		})
	}
}

func TestUpgradeOriginProxyBoundary(t *testing.T) {
	r := upgradeRequest()
	r.Header.Set("Origin", "https://example.test")
	r.Header.Set("Forwarded", `for=127.0.0.1;host=example.test;proto=https`)
	r.Header.Set("X-Forwarded-Host", "example.test")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Port", "443")
	r.URL.Scheme = "https"
	if _, err := originUpgrade(t, r); err != ErrOriginNotAllowed {
		t.Fatalf("spoofed headers allowed: %v", err)
	}
	if _, err := originUpgrade(t, r, originPolicy(func(_ *http.Request, o string) bool { return o == "https://example.test" })); err != nil {
		t.Fatal(err)
	}
	// Application-owned trust fixture: only a verified ingress supplies context.
	// No forwarding header is ever promoted directly into trusted context.
	type publicOriginKey struct{}
	policy := originPolicy(func(r *http.Request, o string) bool {
		expected, ok := r.Context().Value(publicOriginKey{}).(string)
		return ok && o == expected
	})
	installIngressContext := func(r *http.Request) *http.Request {
		// This fixture's configured ingress is one exact address. A real
		// deployment must also prevent untrusted access through that ingress.
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || host != "192.0.2.10" {
			return r
		}
		return r.WithContext(context.WithValue(r.Context(), publicOriginKey{}, "https://example.test"))
	}
	for _, tc := range []struct {
		peer, origin string
		allow        bool
	}{
		{"198.51.100.20:1234", "https://example.test", false},
		{"192.0.2.10:1234", "https://example.test", true},
		{"192.0.2.10:1234", "https://evil.test", false},
	} {
		r2 := r.Clone(t.Context())
		r2.RemoteAddr = tc.peer
		r2.Header.Set("Origin", tc.origin)
		_, err := originUpgrade(t, installIngressContext(r2), policy)
		if (err == nil) != tc.allow {
			t.Fatalf("ingress %s origin %s: %v", tc.peer, tc.origin, err)
		}
	}
}

func TestUpgradeOriginRealLoopback(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP", true: "HTTPS"}[secure], func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := Upgrade(w, r) // Upgrade writes the 403 itself.
				if err != nil {
					return
				}
				defer c.Close()
				if err := c.WriteMessage(TextMessage, []byte("ok")); err != nil {
					t.Error(err)
				}
			}))
			if secure {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			for _, origin := range []string{"", srv.URL, "http://other.test", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
				d := &Dialer{Header: http.Header{"Origin": {origin}}}
				if origin == "" {
					d.Header = nil
				}
				if secure {
					d.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				c, resp, err := d.Dial(ctx, "ws"+strings.TrimPrefix(u.Scheme, "http")+"://"+u.Host)
				allowed := origin == "" || origin == srv.URL
				if allowed {
					if err != nil {
						t.Fatal(err)
					}
					_, p, e := c.ReadMessage()
					c.Close()
					if e != nil || string(p) != "ok" {
						t.Fatalf("message %q %v", p, e)
					}
				} else if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
					t.Fatalf("origin %q: %v, %v", origin, resp, err)
				}
			}
		})
	}
}

func FuzzCanonicalOrigin(f *testing.F) {
	for _, s := range []string{"null", "http://example.test", "HTTPS://EXAMPLE.TEST:00443", "http://[::ffff:127.0.0.1]", "http://example.test?", "http://example.test:65536", "http://exa\tmple.test", "custom://host:0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 4096 {
			t.Skip()
		}
		got, ok := canonicalOrigin(value)
		again, ok2 := canonicalOrigin(value)
		if got != again || ok != ok2 {
			t.Fatal("nondeterministic parse")
		}
		if !ok {
			return
		}
		if rt, valid := canonicalOrigin(got); !valid || rt != got {
			t.Fatalf("not canonical: %q", got)
		}
		if got == "null" {
			return
		}
		u, err := url.Parse(got)
		if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery {
			t.Fatalf("invalid accepted origin %q", got)
		}
		if strings.ContainsAny(got, "\\,?# \t\r\n") {
			t.Fatalf("ambiguous origin %q", got)
		}
	})
}

func TestUpgradeOriginDenialHasNoSideEffects(t *testing.T) {
	for _, tc := range []struct {
		origin string
		check  func(*http.Request, string) bool
	}{
		{"https://evil.test", nil}, {"null", nil},
		{"http://example.test", func(*http.Request, string) bool { return false }},
		{"<missing>", func(*http.Request, string) bool { return false }},
	} {
		r := upgradeRequest()
		if tc.origin != "<missing>" {
			r.Header.Set("Origin", tc.origin)
		}
		w := new(originWriter)
		ext := &negotiationExtension{name: "example", offer: "example"}
		c, err := (&Upgrader{CheckOrigin: tc.check, newExtensions: func() []frameExtension { return []frameExtension{ext} }}).Upgrade(w, r)
		if c != nil || err != ErrOriginNotAllowed || w.calls != 0 || ext.calls != 0 || w.status != http.StatusForbidden {
			t.Fatalf("rejection: conn %v err %v hijacks %d extension %d status %d", c, err, w.calls, ext.calls, w.status)
		}
	}
	r := upgradeRequest()
	r.Header["oRiGiN"] = []string{"http://example.test"}
	if _, err := originUpgrade(t, r); err != nil {
		t.Fatalf("single noncanonical header key: %v", err)
	}
}
