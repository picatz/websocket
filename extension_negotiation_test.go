package websocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseExtensions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		valid  bool
	}{
		{"absent", nil, true},
		{"simple", []string{"example"}, true},
		{"parameters", []string{`example; flag; value=token; quoted="to\ken"`}, true},
		{"whitespace", []string{" \texample \t; flag \t; value \t= \t token \t"}, true},
		{"repeated fields", []string{"example", "second; flag"}, true},
		{"alternatives", []string{"example; option=1, example; option=2"}, true},
		{"generic duplicate parameter", []string{"example; option=1; option=2"}, true},
		{"empty list elements", []string{", example, ,", "", "second,"}, true},
		{"empty", []string{""}, false},
		{"only separators", []string{", ,", " \t"}, false},
		{"quoted name", []string{`"example"`}, false},
		{"name separator", []string{"exam/ple"}, false},
		{"name whitespace", []string{"exam ple"}, false},
		{"unicode whitespace", []string{"\u00a0example"}, false},
		{"nonascii", []string{"exámple"}, false},
		{"trailing semicolon", []string{"example;"}, false},
		{"empty parameter", []string{"example;; flag"}, false},
		{"quoted parameter name", []string{`example; "flag"`}, false},
		{"missing value", []string{"example; option="}, false},
		{"double equals", []string{"example; option==1"}, false},
		{"empty quoted value", []string{`example; option=""`}, false},
		{"unterminated quote", []string{`example; option="token`}, false},
		{"unterminated escape", []string{`example; option="token\`}, false},
		{"quoted non-token", []string{`example; option="a b"`}, false},
		{"quoted comma", []string{`example; option="a,b"`}, false},
		{"quoted semicolon", []string{`example; option="a;b"`}, false},
		{"escaped quote", []string{`example; option="a\"b"`}, false},
		{"escaped slash", []string{`example; option="a\/b"`}, false},
		{"trailing quoted text", []string{`example; option="token"suffix`}, false},
		{"cross-field quoted value", []string{`example; option="a`, `b"`}, false},
		{"CRLF injection", []string{"example\r\nInjected: true"}, false},
		{"LF injection", []string{"example\nInjected: true"}, false},
		{"control", []string{"example; option=a\x00b"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseExtensions(tc.values)
			if (err == nil) != tc.valid || err != nil && !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("parseExtensions(%q) = %v, valid = %v", tc.values, err, tc.valid)
			}
			if !tc.valid {
				r := upgradeRequest()
				r.Header["Sec-WebSocket-Extensions"] = tc.values
				w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
				conn, err := Upgrade(w, r) // Validate even without installed extensions.
				if conn != nil || !errors.Is(err, ErrInvalidExtension) || w.calls != 0 || w.Body.Len() != 0 {
					t.Fatalf("Upgrade = %v, %v; hijacks = %d, body = %q", conn, err, w.calls, w.Body.String())
				}
			}
		})
	}
	got, err := parseExtensions([]string{`first; a="to\ken"; flag, second; a=other`})
	want := []extensionOffer{
		{name: "first", params: []extensionParameter{{name: "a", value: "token", hasValue: true}, {name: "flag"}}},
		{name: "second", params: []extensionParameter{{name: "a", value: "other", hasValue: true}}},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed structure = %#v, %v; want %#v", got, err, want)
	}
}

// This no-op custom extension intentionally owns its negotiation semantics.
// The library must pass its complete header through rather than infer them.
type negotiationExtension struct {
	name, offer, received string
	enabled               bool
	calls                 int
	err                   error
}

func (e *negotiationExtension) Name() string                      { return e.name }
func (e *negotiationExtension) Offer() string                     { return e.offer }
func (e *negotiationExtension) IsEnabled() bool                   { return e.enabled }
func (e *negotiationExtension) ProcessOutgoingFrame(*Frame) error { return nil }
func (e *negotiationExtension) ProcessIncomingFrame(*Frame) error { return nil }
func (e *negotiationExtension) Negotiate(header string) error {
	e.received = header
	e.calls++
	return e.err
}

func TestDialExtensionSelection(t *testing.T) {
	callbackError := errors.New("synthetic extension rejection")
	for _, tc := range []struct {
		name      string
		offers    []string
		selection []string
		callback  error
		builtin   bool
		valid     bool
	}{
		{name: "no extensions", valid: true},
		{name: "declined", offers: []string{"example"}, valid: true},
		{name: "custom", offers: []string{"example; offer=1"}, selection: []string{`example; selected="2"`}, valid: true},
		{name: "multiple fields", offers: []string{"example", "second"}, selection: []string{"example", "second"}, valid: true},
		{name: "offered alternatives", offers: []string{"example; option=1, example; option=2"}, selection: []string{"example; option=2"}, valid: true},
		{name: "custom repetitions delegated", offers: []string{"example"}, selection: []string{"example; option=1; option=2, example"}, valid: true},
		{name: "unsolicited PMD", selection: []string{"permessage-deflate"}},
		{name: "unsolicited custom", selection: []string{"unknown"}},
		{name: "unoffered", offers: []string{"example"}, selection: []string{"other"}},
		{name: "additional unoffered", offers: []string{"example"}, selection: []string{"example, other"}},
		{name: "second field unoffered", offers: []string{"example"}, selection: []string{"example", "other"}},
		{name: "case sensitive", offers: []string{"example"}, selection: []string{"Example"}},
		{name: "prefix", offers: []string{"permessage-deflate"}, selection: []string{"x-permessage-deflate"}},
		{name: "suffix", offers: []string{"permessage-deflate"}, selection: []string{"permessage-deflate-x"}},
		{name: "duplicate PMD", offers: []string{"permessage-deflate"}, selection: []string{"permessage-deflate, permessage-deflate"}},
		{name: "conflicting PMD", offers: []string{"permessage-deflate"}, selection: []string{"permessage-deflate; server_max_window_bits=10", "permessage-deflate; server_max_window_bits=15"}},
		{name: "empty field", offers: []string{"example"}, selection: []string{""}},
		{name: "malformed second field", offers: []string{"example"}, selection: []string{"example", "other;"}},
		{name: "bad quote", offers: []string{"example"}, selection: []string{`example; option="a,b"`}},
		{name: "callback error", offers: []string{"example"}, selection: []string{"example"}, callback: callbackError},
		{name: "builtin quoted selection", offers: []string{"permessage-deflate"}, selection: []string{`permessage-deflate; server_max_window_bits="15"`}, builtin: true, valid: true},
		{name: "builtin declined", offers: []string{"permessage-deflate"}, builtin: true, valid: true},
		{name: "builtin duplicate parameter", offers: []string{"permessage-deflate"}, selection: []string{"permessage-deflate; server_max_window_bits=10; server_max_window_bits=15"}, builtin: true},
		{name: "builtin unknown parameter", offers: []string{"permessage-deflate"}, selection: []string{"permessage-deflate; unknown"}, builtin: true},
		{name: "builtin invalid flag", offers: []string{"permessage-deflate"}, selection: []string{"permessage-deflate; client_no_context_takeover=1"}, builtin: true},
		{name: "builtin missing window value", offers: []string{"permessage-deflate"}, selection: []string{"permessage-deflate; client_max_window_bits"}, builtin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peerClosed := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.Header.Get("Sec-WebSocket-Extensions"), strings.Join(tc.offers, ", "); got != want {
					t.Errorf("request offers = %q, want %q", got, want)
				}
				raw, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					peerClosed <- err
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(3 * time.Second))
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
				for _, value := range tc.selection {
					fmt.Fprintf(rw, "Sec-WebSocket-Extensions: %s\r\n", value)
				}
				rw.WriteString("\r\n")
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
			var extensions []Extension
			for _, offer := range tc.offers {
				if tc.builtin {
					extensions = append(extensions, NewPerMessageDeflateExtension())
					continue
				}
				extensions = append(extensions, &negotiationExtension{name: "synthetic", offer: offer, err: tc.callback})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			conn, resp, err := Dial(ctx, "ws"+srv.URL[4:], WithExtensions(extensions...))
			if conn != nil {
				defer conn.conn.Close()
			}
			if (err == nil) != tc.valid || resp == nil {
				t.Fatalf("Dial = %v, %v, %v; valid = %v", conn, resp, err, tc.valid)
			}
			if tc.valid {
				opcode, data, err := conn.ReadMessage()
				if err != nil || opcode != BinaryMessage || string(data) != "x" {
					t.Fatalf("buffered message = %v, %q, %v", opcode, data, err)
				}
				conn.conn.Close()
			} else if conn != nil || !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("invalid selection = %v, %v", conn, err)
			}
			if tc.callback != nil && !errors.Is(err, tc.callback) {
				t.Fatalf("lost callback error: %v", err)
			}
			for _, ext := range extensions {
				e, custom := ext.(*negotiationExtension)
				if !custom {
					continue
				}
				wantCalls := 0
				if tc.valid || tc.callback != nil {
					wantCalls = 1
				}
				if e.calls != wantCalls || wantCalls == 1 && e.received != strings.Join(tc.selection, ", ") {
					t.Fatalf("callback = %d, %q", e.calls, e.received)
				}
			}
			if err := <-peerClosed; !errors.Is(err, io.EOF) {
				t.Fatalf("transport not closed: %v", err)
			}
		})
	}
}

func TestDialRejectsInvalidExtensionOfferBeforeConnect(t *testing.T) {
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
			conn.Close()
		}
	}()
	defer func() { listener.Close(); <-done }()
	for _, offer := range []string{"", "example;", "example\r\nInjected: true", `example; option="a,b"`} {
		ext := &negotiationExtension{name: "example", offer: offer}
		conn, resp, err := Dial(t.Context(), "ws://"+listener.Addr().String(), WithExtensions(ext))
		if conn != nil || resp != nil || !errors.Is(err, ErrInvalidExtension) || !errors.Is(err, ErrInvalidHandshakeHeader) || connects.Load() != 0 || ext.calls != 0 {
			t.Fatalf("offer %q: Dial = %v, %v, %v; connects = %d, callbacks = %d", offer, conn, resp, err, connects.Load(), ext.calls)
		}
	}
}

type extensionHijacker struct {
	*hijackResponse
	calls int
}

func (w *extensionHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	return w.hijackResponse.Hijack()
}

func TestUpgradeExtensionSelection(t *testing.T) {
	callbackError := errors.New("synthetic server extension rejection")
	for _, tc := range []struct {
		name      string
		offers    []string
		selection string
		callback  error
		valid     bool
	}{
		{name: "custom", offers: []string{"example; option=1", "second"}, selection: "example; option=2", valid: true},
		{name: "custom repetitions", offers: []string{"example"}, selection: "example; a=1; a=2, example", valid: true},
		{name: "unsolicited", selection: "example"},
		{name: "unoffered", offers: []string{"example"}, selection: "other"},
		{name: "extra unoffered", offers: []string{"example"}, selection: "example, other"},
		{name: "malformed", offers: []string{"example"}, selection: "example;"},
		{name: "injected", offers: []string{"example"}, selection: "example\r\nInjected: true"},
		{name: "duplicate PMD", offers: []string{"permessage-deflate"}, selection: "permessage-deflate, permessage-deflate"},
		{name: "callback error", offers: []string{"example"}, selection: "example", callback: callbackError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := upgradeRequest()
			r.Header["Sec-WebSocket-Extensions"] = tc.offers
			before := r.Header.Clone()
			ext := &negotiationExtension{name: "example", offer: tc.selection, enabled: true, err: tc.callback}
			raw := &memoryConn{Reader: bytes.NewReader(nil)}
			w := &extensionHijacker{hijackResponse: &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}}
			conn, err := Upgrade(w, r, WithUpgradeExtensions(ext))
			if (err == nil) != tc.valid {
				t.Fatalf("Upgrade = %v, %v; valid = %v", conn, err, tc.valid)
			}
			if tc.valid {
				conn.conn.Close()
				resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw.written.String())), nil)
				if err != nil || resp.Header.Get("Sec-WebSocket-Extensions") != tc.selection {
					t.Fatalf("response = %v, %v", resp, err)
				}
			} else if conn != nil || !errors.Is(err, ErrInvalidExtension) || raw.written.Len() != 0 || w.calls != 0 {
				t.Fatalf("invalid selection = %v, %v; response = %q", conn, err, raw.written.String())
			}
			if tc.callback != nil && !errors.Is(err, tc.callback) {
				t.Fatalf("lost callback error: %v", err)
			}
			if ext.calls != 1 || ext.received != strings.Join(tc.offers, ", ") || !reflect.DeepEqual(r.Header, before) {
				t.Fatalf("callback = %d, %q; mutated headers = %v", ext.calls, ext.received, !reflect.DeepEqual(r.Header, before))
			}
		})
	}
}
