package websocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func invalidCustomHeaders(response bool) map[string]http.Header {
	cases := map[string]http.Header{
		"empty name":           {"": {"value"}},
		"space name":           {"Bad Name": {"value"}},
		"colon name":           {"Bad:Name": {"value"}},
		"non-ASCII name":       {"X-É": {"value"}},
		"CRLF name":            {"X-Test\r\nInjected": {"value"}},
		"CRLF value":           {"X-Test": {"ok\r\nInjected: true"}},
		"CR value":             {"X-Test": {"a\rb"}},
		"LF value":             {"X-Test": {"a\nb"}},
		"NUL value":            {"X-Test": {"a\x00b"}},
		"DEL value":            {"X-Test": {"a\x7fb"}},
		"control value":        {"X-Test": {"a\x1fb"}},
		"second value invalid": {"X-Test": {"valid", "invalid\nvalue"}},
		"nil invalid name":     {"Bad Name": nil},
	}
	reserved := []string{"Upgrade", "Connection", "Sec-WebSocket-Extensions", "Content-Length", "Transfer-Encoding"}
	if response {
		reserved = append(reserved, "Sec-WebSocket-Accept")
	} else {
		reserved = append(reserved, "Host", "Sec-WebSocket-Key", "Sec-WebSocket-Version")
	}
	for _, name := range reserved {
		for _, variant := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
			cases["reserved "+variant] = http.Header{variant: {"value"}}
		}
		cases["nil reserved "+name] = http.Header{name: nil}
	}
	return cases
}

type countingHijacker struct {
	*httptest.ResponseRecorder
	calls int
}

func (w *countingHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	return nil, nil, errors.New("unexpected hijack")
}

func TestUpgradeRejectsCustomHeadersBeforeHijack(t *testing.T) {
	for name, headers := range invalidCustomHeaders(true) {
		t.Run(name, func(t *testing.T) {
			before := headers.Clone()
			w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
			conn, err := Upgrade(w, upgradeRequest(), WithResponseHeader(headers))
			if conn != nil || !errors.Is(err, ErrInvalidHandshakeHeader) {
				t.Fatalf("Upgrade = %v, %v", conn, err)
			}
			if w.calls != 0 || w.Body.Len() != 0 {
				t.Fatalf("invalid fields hijacked %d times or wrote %q", w.calls, w.Body.String())
			}
			if !reflect.DeepEqual(headers, before) {
				t.Fatal("mutated caller headers")
			}
		})
	}
}

func TestDialRejectsCustomHeadersBeforeConnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connects atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connects.Add(1)
			// A regressed Dial must finish promptly, even for malformed requests.
			conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
			conn.Close()
		}
	}()
	defer func() { listener.Close(); <-done }()
	for name, headers := range invalidCustomHeaders(false) {
		t.Run(name, func(t *testing.T) {
			before := headers.Clone()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			conn, resp, err := Dial(ctx, "ws://"+listener.Addr().String(), WithHeader(headers))
			if conn != nil || resp != nil || !errors.Is(err, ErrInvalidHandshakeHeader) {
				t.Fatalf("Dial = %v, %v, %v", conn, resp, err)
			}
			if connects.Load() != 0 {
				t.Fatalf("connected %d times for invalid fields", connects.Load())
			}
			if !reflect.DeepEqual(headers, before) {
				t.Fatal("mutated caller headers")
			}
		})
	}
}

func TestCustomHeaderErrorsDoNotExposeValues(t *testing.T) {
	const secret = "secret-cookie\nvalue"
	for _, response := range []bool{false, true} {
		err := validateHandshakeHeaders(http.Header{"Cookie": {secret}}, response)
		if !errors.Is(err, ErrInvalidHandshakeHeader) || strings.Contains(err.Error(), secret) {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestValidCustomHeaderCompatibility(t *testing.T) {
	requestHeaders := http.Header{
		"authorization":          {"Bearer example"},
		"X-Repeated":             {"one", "two"},
		"x-lower":                {"lower"},
		"X-Empty":                {""},
		"X-Omitted":              nil,
		"X-Text":                 {"tab\tand \xff"},
		"!#$%&'*+-.^_`|~":        {"token alphabet"},
		"Sec-WebSocket-Protocol": {"example"},
	}
	responseHeaders := http.Header{"Set-Cookie": {"a=1; Path=/", "b=2; Path=/"}, "x-custom": {"one", "two"}, "Sec-WebSocket-Protocol": {"example"}}
	requestBefore, responseBefore := requestHeaders.Clone(), responseHeaders.Clone()
	received := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		conn, err := Upgrade(w, r, WithResponseHeader(responseHeaders))
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.conn.Close()
	}))
	defer srv.Close()
	conn, resp, err := Dial(t.Context(), "ws"+srv.URL[4:], WithHeader(requestHeaders))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.conn.Close()
	got := <-received
	for name, values := range requestHeaders {
		if !reflect.DeepEqual(got.Values(name), values) {
			t.Errorf("request %q = %#v, want %#v", name, got.Values(name), values)
		}
	}
	for name, values := range responseHeaders {
		if !reflect.DeepEqual(resp.Header.Values(name), values) {
			t.Errorf("response %q = %#v, want %#v", name, resp.Header.Values(name), values)
		}
	}
	if !reflect.DeepEqual(requestHeaders, requestBefore) || !reflect.DeepEqual(responseHeaders, responseBefore) {
		t.Fatal("mutated caller headers")
	}
}

func TestUpgradePreservesRepeatedHeaderWireLines(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
	_, err := Upgrade(w, upgradeRequest(), WithResponseHeader(http.Header{"Set-Cookie": {"a=1", "b=2"}, "x-test": {"one", "two"}}))
	if err != nil {
		t.Fatal(err)
	}
	wire := raw.written.String()
	for _, line := range []string{"Set-Cookie: a=1\r\n", "Set-Cookie: b=2\r\n", "x-test: one\r\n", "x-test: two\r\n"} {
		if strings.Count(wire, line) != 1 {
			t.Errorf("missing/repeated line %q in %q", line, wire)
		}
	}
}

func TestHandshakeHeaderByteAlphabet(t *testing.T) {
	for i := 0; i <= 255; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			value := string([]byte{byte(i)})
			err := validateHandshakeHeaders(http.Header{"X-Test": {value}}, false)
			allowed := i == '\t' || i >= 32 && i != 127
			if (err == nil) != allowed {
				t.Fatalf("value byte %d: %v", i, err)
			}
			name := "X" + value
			err = validateHandshakeHeaders(http.Header{name: {"ok"}}, false)
			allowed = i >= 'a' && i <= 'z' || i >= 'A' && i <= 'Z' || i >= '0' && i <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(i))
			if (err == nil) != allowed {
				t.Fatalf("name byte %d: %v", i, err)
			}
		})
	}
}
