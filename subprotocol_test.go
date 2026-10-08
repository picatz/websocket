package websocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDialRejectsUnsolicitedSubprotocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer raw.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: unoffered\r\n\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		rw.Flush()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, _, err := Dial(ctx, "ws"+srv.URL[4:])
	if conn != nil {
		conn.conn.Close()
	}
	if err == nil {
		t.Fatal("accepted an unsolicited subprotocol")
	}
}

func TestUpgradeRejectsUnofferedSubprotocol(t *testing.T) {
	w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
	_, _ = Upgrade(w, upgradeRequest(), WithResponseHeader(http.Header{"Sec-WebSocket-Protocol": {"unoffered"}}))
	if w.calls != 0 {
		t.Fatal("attempted hijack with an unoffered subprotocol")
	}
}

func TestSubprotocolOfferValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		valid  bool
	}{
		{"absent", nil, true}, {"nil slice", http.Header{"Sec-WebSocket-Protocol": nil}, true},
		{"single", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, true},
		{"list", http.Header{"Sec-WebSocket-Protocol": {" chat ,\tsuperchat "}}, true},
		{"repeated mixed keys", http.Header{"Sec-WebSocket-Protocol": {"chat"}, "sec-websocket-protocol": {"CHAT", "superchat"}}, true},
		{"empty list elements", http.Header{"Sec-WebSocket-Protocol": {", chat, ,"}}, true},
		{"empty", http.Header{"Sec-WebSocket-Protocol": {""}}, false},
		{"whitespace", http.Header{"Sec-WebSocket-Protocol": {" \t"}}, false},
		{"only commas", http.Header{"Sec-WebSocket-Protocol": {", ,"}}, false},
		{"duplicate list", http.Header{"Sec-WebSocket-Protocol": {"chat,chat"}}, false},
		{"duplicate keys", http.Header{"Sec-WebSocket-Protocol": {"chat"}, "SEC-WEBSOCKET-PROTOCOL": {"chat"}}, false},
		{"quoted", http.Header{"Sec-WebSocket-Protocol": {`"chat"`}}, false},
		{"separator", http.Header{"Sec-WebSocket-Protocol": {"chat/1"}}, false},
		{"space", http.Header{"Sec-WebSocket-Protocol": {"chat one"}}, false},
		{"nonascii", http.Header{"Sec-WebSocket-Protocol": {"chât"}}, false},
		{"unicode whitespace", http.Header{"Sec-WebSocket-Protocol": {"\u00a0chat"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.header.Clone()
			_, err := parseSubprotocolOffers(tc.header)
			if (err == nil) != tc.valid {
				t.Fatalf("parse error = %v, valid = %v", err, tc.valid)
			}
			if !reflect.DeepEqual(tc.header, before) {
				t.Fatal("mutated headers")
			}
			if !tc.valid {
				// An invalid URL must not mask caller-header validation, which runs before dialing.
				conn, resp, err := Dial(t.Context(), ":invalid", WithHeader(tc.header))
				if conn != nil || resp != nil || !errors.Is(err, ErrInvalidSubprotocol) || !errors.Is(err, ErrInvalidHandshakeHeader) {
					t.Fatalf("Dial = %v, %v, %v", conn, resp, err)
				}
				r := upgradeRequest()
				r.Header = mergeHeaders(r.Header, tc.header)
				w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
				conn, err = Upgrade(w, r)
				if conn != nil || !errors.Is(err, ErrInvalidSubprotocol) || w.calls != 0 {
					t.Fatalf("Upgrade = %v, %v, hijacks %d", conn, err, w.calls)
				}
			}
		})
	}
}

func mergeHeaders(a, b http.Header) http.Header {
	result := a.Clone()
	for k, v := range b {
		result[k] = v
	}
	return result
}

func TestSubprotocolSelectionHandshake(t *testing.T) {
	for _, tc := range []struct {
		name             string
		offer, selection http.Header
		valid            bool
	}{
		{"none", nil, nil, true},
		{"declined", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, nil, true},
		{"selected second", http.Header{"sec-websocket-protocol": {"chat, superchat"}}, http.Header{"sec-websocket-protocol": {"\tsuperchat "}}, true},
		{"repeated offers", http.Header{"Sec-WebSocket-Protocol": {"chat", "superchat"}}, http.Header{"Sec-WebSocket-Protocol": {"superchat"}}, true},
		{"unsolicited", nil, http.Header{"Sec-WebSocket-Protocol": {"chat"}}, false},
		{"unoffered", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, http.Header{"Sec-WebSocket-Protocol": {"other"}}, false},
		{"case sensitive", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, http.Header{"Sec-WebSocket-Protocol": {"CHAT"}}, false},
		{"multiple", http.Header{"Sec-WebSocket-Protocol": {"chat, superchat"}}, http.Header{"Sec-WebSocket-Protocol": {"chat, superchat"}}, false},
		{"repeated", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, http.Header{"Sec-WebSocket-Protocol": {"chat", "chat"}}, false},
		{"mixed repeated", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, http.Header{"Sec-WebSocket-Protocol": {"chat"}, "sec-websocket-protocol": {"chat"}}, false},
		{"empty", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, http.Header{"Sec-WebSocket-Protocol": {""}}, false},
		{"trailing comma", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, http.Header{"Sec-WebSocket-Protocol": {"chat,"}}, false},
		{"quoted", http.Header{"Sec-WebSocket-Protocol": {"chat"}}, http.Header{"Sec-WebSocket-Protocol": {`"chat"`}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeOffer, beforeSelection := tc.offer.Clone(), tc.selection.Clone()
			raw := &memoryConn{Reader: bytes.NewReader(nil)}
			w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
			r := upgradeRequest()
			r.Header = mergeHeaders(r.Header, tc.offer)
			conn, err := Upgrade(w, r, WithResponseHeader(tc.selection))
			if (err == nil) != tc.valid {
				t.Fatalf("Upgrade error = %v, valid = %v", err, tc.valid)
			}
			if conn != nil {
				conn.conn.Close()
			}
			if !tc.valid && (!errors.Is(err, ErrInvalidSubprotocol) || !errors.Is(err, ErrInvalidHandshakeHeader) || raw.written.Len() != 0) {
				t.Fatalf("Upgrade error %v, wrote %q", err, raw.written.String())
			}
			peerClosed := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(2 * time.Second))
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
				for name, values := range tc.selection {
					for _, value := range values {
						fmt.Fprintf(rw, "%s: %s\r\n", name, value)
					}
				}
				rw.WriteString("\r\n")
				rw.Write([]byte{0x81, 1, 'x'})
				rw.Flush()
				var b [1]byte
				_, err = rw.Read(b[:])
				peerClosed <- err
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			conn, resp, err := Dial(ctx, "ws"+srv.URL[4:], WithHeader(tc.offer))
			if (err == nil) != tc.valid || resp == nil {
				t.Fatalf("Dial = %v, %v, %v", conn, resp, err)
			}
			if conn != nil {
				_, data, readErr := conn.ReadMessage()
				if readErr != nil || string(data) != "x" {
					t.Fatalf("buffered message %q, %v", data, readErr)
				}
				conn.conn.Close()
			}
			if !tc.valid && (conn != nil || !errors.Is(err, ErrInvalidSubprotocol)) {
				t.Fatalf("rejected Dial = %v, %v", conn, err)
			}
			if err := <-peerClosed; err != io.EOF {
				t.Fatalf("transport not closed cleanly: %v", err)
			}
			if !reflect.DeepEqual(tc.offer, beforeOffer) || !reflect.DeepEqual(tc.selection, beforeSelection) {
				t.Fatal("mutated caller headers")
			}
		})
	}
}

func TestSubprotocolTokenByteAlphabet(t *testing.T) {
	for i := 0; i < 256; i++ {
		token := "x" + string([]byte{byte(i)}) + "y"
		allowed := i >= 'a' && i <= 'z' || i >= 'A' && i <= 'Z' || i >= '0' && i <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(i))
		// Comma is a list delimiter in offers, but never valid inside a selection.
		offered, err := parseSubprotocolOffers(http.Header{"Sec-WebSocket-Protocol": {token}})
		if (err == nil) != (allowed || i == ',') {
			t.Fatalf("offer byte %d: %v", i, err)
		}
		err = validateSubprotocolSelection(http.Header{"Sec-WebSocket-Protocol": {token}}, map[string]struct{}{token: {}})
		if (err == nil) != allowed {
			t.Fatalf("selection byte %d: %v", i, err)
		}
		if allowed {
			if err := validateSubprotocolSelection(http.Header{"Sec-WebSocket-Protocol": {token}}, offered); err != nil {
				t.Fatalf("round trip byte %d: %v", i, err)
			}
		}
	}
}

func FuzzSubprotocolNegotiation(f *testing.F) {
	for _, seed := range []string{"chat", "chat, superchat", "", "chat,chat", "\tchat ", "chat/1", "!#$%&'*+-.^_`|~"} {
		f.Add(seed, "chat")
	}
	f.Fuzz(func(t *testing.T, offer, selected string) {
		offered, err := parseSubprotocolOffers(http.Header{"Sec-WebSocket-Protocol": {offer}})
		if err != nil {
			return
		}
		err = validateSubprotocolSelection(http.Header{"Sec-WebSocket-Protocol": {selected}}, offered)
		if err == nil {
			token := strings.Trim(selected, " \t")
			if !isHTTPToken(token) {
				t.Fatal("accepted invalid token")
			}
			if _, ok := offered[token]; !ok {
				t.Fatal("accepted unoffered token")
			}
		}
	})
}
