package websocket

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// fuzzHasToken reports whether any comma-separated element of the header's
// values equals token, ignoring ASCII case and optional whitespace (RFC 9110
// Section 5.6.1). It is written independently of the package's parser.
func fuzzHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for elem := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.Trim(elem, " \t"), token) {
				return true
			}
		}
	}
	return false
}

// FuzzUpgrade feeds arbitrary HTTP requests to Upgrader.Upgrade and checks
// the outcome against RFC 6455 Section 4.2: an accepted request met every
// requirement of Section 4.2.1 and was answered with a 101 whose
// Sec-WebSocket-Accept is derived from its key, and a rejected one reports an
// error callers can classify and has been answered with the matching status
// without the connection being taken over.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-4.2
func FuzzUpgrade(f *testing.F) {
	const valid = "GET /ws HTTP/1.1\r\nHost: example.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for _, seed := range []string{
		valid + "\r\n",
		valid + "Origin: http://example.test\r\n\r\n",
		valid + "Origin: http://evil.test\r\n\r\n",
		valid + "Sec-WebSocket-Protocol: chat, superchat\r\n\r\n",
		valid + "Sec-WebSocket-Protocol: chat,,\r\n\r\n",
		valid + "Sec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n\r\n",
		valid + "Sec-WebSocket-Extensions: permessage-deflate; server_max_window_bits=7\r\n\r\n",
		valid + "Sec-WebSocket-Extensions: x-unknown, permessage-deflate;;\r\n\r\n",
		valid + "Sec-WebSocket-Version: 13\r\n\r\n",
		valid + "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		strings.Replace(valid, "13", "8", 1) + "\r\n",
		strings.Replace(valid, "GET", "POST", 1) + "\r\n",
		strings.Replace(valid, "HTTP/1.1", "HTTP/1.0", 1) + "\r\n",
		strings.Replace(valid, "Connection: Upgrade", "Connection: keep-alive, Upgrade", 1) + "\r\n",
		strings.Replace(valid, "Connection: Upgrade", "Connection: Upgradex", 1) + "\r\n",
		strings.Replace(valid, "dGhlIHNhbXBsZSBub25jZQ==", "dGhlIHNhbXBsZSBub25jZQ", 1) + "\r\n",
		strings.Replace(valid, "dGhlIHNhbXBsZSBub25jZQ==", "AAAAAAAAAAAAAAAAAAAAAAAA", 1) + "\r\n",
	} {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, raw string, configured bool) {
		r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			return
		}
		var u Upgrader
		if configured {
			u = Upgrader{Subprotocols: []string{"chat", "superchat"}, Compression: &Compression{}}
		}
		conn := &memoryConn{Reader: bytes.NewReader(nil)}
		w := &hijackResponse{httptest.NewRecorder(), conn, bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))}
		c, err := u.Upgrade(w, r)

		if err != nil {
			if c != nil {
				t.Fatal("Upgrade returned a Conn with an error")
			}
			if conn.written.Len() != 0 {
				t.Fatalf("rejected request wrote to the connection: %q", conn.written.Bytes())
			}
			want := map[int]bool{http.StatusBadRequest: true, http.StatusMethodNotAllowed: true, http.StatusUpgradeRequired: true}
			switch {
			case errors.Is(err, ErrOriginNotAllowed):
				want = map[int]bool{http.StatusForbidden: true}
			case errors.Is(err, ErrUnsupportedVersion):
				want = map[int]bool{http.StatusUpgradeRequired: true}
				if got := w.Header().Get("Sec-WebSocket-Version"); got != "13" {
					t.Fatalf("426 advertises Sec-WebSocket-Version %q, want 13", got)
				}
			case errors.Is(err, ErrBadHandshake):
			default:
				t.Fatalf("err = %v, want ErrBadHandshake or ErrOriginNotAllowed", err)
			}
			if !want[w.Code] {
				t.Fatalf("err = %v answered with status %d", err, w.Code)
			}
			return
		}
		defer conn.Close()

		// RFC 6455 Section 4.2.1: what the server must find in the request.
		key := r.Header.Get("Sec-WebSocket-Key")
		decoded, _ := base64.StdEncoding.DecodeString(key)
		switch {
		case r.Method != http.MethodGet || !r.ProtoAtLeast(1, 1):
			t.Fatalf("accepted %s %s", r.Method, r.Proto)
		case !fuzzHasToken(r.Header, "Upgrade", "websocket"):
			t.Fatalf("accepted Upgrade %q", r.Header.Values("Upgrade"))
		case !fuzzHasToken(r.Header, "Connection", "Upgrade"):
			t.Fatalf("accepted Connection %q", r.Header.Values("Connection"))
		case !slices.Equal(r.Header.Values("Sec-WebSocket-Version"), []string{"13"}):
			t.Fatalf("accepted Sec-WebSocket-Version %q", r.Header.Values("Sec-WebSocket-Version"))
		case len(r.Header.Values("Sec-WebSocket-Key")) != 1 || len(decoded) != 16:
			t.Fatalf("accepted Sec-WebSocket-Key %q", r.Header.Values("Sec-WebSocket-Key"))
		}

		// RFC 6455 Section 4.2.2: the 101 response.
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(conn.written.Bytes())), r)
		if err != nil {
			t.Fatalf("response: %v\n%q", err, conn.written.Bytes())
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("status = %d, want 101", resp.StatusCode)
		}
		sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		if got, want := resp.Header.Get("Sec-WebSocket-Accept"), base64.StdEncoding.EncodeToString(sum[:]); got != want {
			t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
		}
		if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != c.Subprotocol() {
			t.Fatalf("Sec-WebSocket-Protocol = %q, Subprotocol() = %q", got, c.Subprotocol())
		}
		if p := c.Subprotocol(); p != "" && (!slices.Contains(u.Subprotocols, p) || !fuzzHasToken(r.Header, "Sec-WebSocket-Protocol", p)) {
			t.Fatalf("selected subprotocol %q was not offered by both sides", p)
		}
		if ext := resp.Header.Values("Sec-WebSocket-Extensions"); len(ext) != 0 {
			if u.Compression == nil || len(ext) != 1 || !strings.HasPrefix(ext[0], "permessage-deflate") {
				t.Fatalf("Sec-WebSocket-Extensions = %q", ext)
			}
			if !strings.Contains(strings.Join(r.Header.Values("Sec-WebSocket-Extensions"), ","), "permessage-deflate") {
				t.Fatal("accepted an extension the client did not offer")
			}
		}
	})
}
