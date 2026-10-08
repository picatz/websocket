package websocket

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPMDUpgradeDirectionalWindows(t *testing.T) {
	for _, tc := range []struct {
		name, offer, want string
		options           []PerMessageDeflateOption
	}{
		{"default", "permessage-deflate", "permessage-deflate", nil},
		{"bare client", "permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=15", nil},
		{"server 15", "permessage-deflate; server_max_window_bits=15", "permessage-deflate; server_max_window_bits=15", nil},
		{"unsupported then bare", "permessage-deflate; server_max_window_bits=9; client_no_context_takeover, permessage-deflate", "permessage-deflate", nil},
		{"unsupported then supported", "permessage-deflate; server_max_window_bits=14, permessage-deflate; server_max_window_bits=15", "permessage-deflate; server_max_window_bits=15", nil},
		{"unoffered client bound", "permessage-deflate", "", []PerMessageDeflateOption{WithClientMaxWindowBits(9)}},
		{"unoffered client 15 omitted", "permessage-deflate", "permessage-deflate", []PerMessageDeflateOption{WithClientMaxWindowBits(15)}},
		{"unsolicited server 15", "permessage-deflate", "permessage-deflate; server_max_window_bits=15", []PerMessageDeflateOption{WithServerMaxWindowBits(15)}},
		{"peer limit", "permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=9", []PerMessageDeflateOption{WithClientMaxWindowBits(9)}},
		{"lower peer hint", "permessage-deflate; client_max_window_bits=8", "permessage-deflate; client_max_window_bits=8", []PerMessageDeflateOption{WithClientMaxWindowBits(9)}},
		{"higher peer hint", "permessage-deflate; client_max_window_bits=14", "permessage-deflate; client_max_window_bits=9", []PerMessageDeflateOption{WithClientMaxWindowBits(9)}},
		{"no union across offers", "permessage-deflate; client_max_window_bits=9; server_max_window_bits=9, permessage-deflate", "", []PerMessageDeflateOption{WithClientMaxWindowBits(9)}},
		{"fallback permitting peer bound", "permessage-deflate, permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=9", []PerMessageDeflateOption{WithClientMaxWindowBits(9)}},
		{"both flags unsolicited", "permessage-deflate", "permessage-deflate; client_no_context_takeover; server_no_context_takeover", []PerMessageDeflateOption{WithClientNoContextTakeover(), WithServerNoContextTakeover()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkPMDUpgrade(t, tc.offer, tc.want, tc.options...)
		})
	}
	for bits := 8; bits <= 15; bits++ {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			client := fmt.Sprintf("permessage-deflate; client_max_window_bits=%d", bits)
			checkPMDUpgrade(t, client, client)
			server := fmt.Sprintf("permessage-deflate; server_max_window_bits=%d", bits)
			want := ""
			if bits == 15 {
				want = server
			}
			checkPMDUpgrade(t, server, want)
		})
	}
}

func checkPMDUpgrade(t *testing.T, offer, want string, options ...PerMessageDeflateOption) {
	t.Helper()
	r := upgradeRequest()
	r.Header.Set("Sec-WebSocket-Extensions", offer)
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
	pmd := NewPerMessageDeflateExtension(options...).(*perMessageDeflate)
	configured := pmd.configured
	c, err := Upgrade(w, r, WithUpgradeExtensions(pmd))
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	response, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw.written.String())), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("Sec-WebSocket-Extensions"); got != want || pmd.IsEnabled() != (want != "") {
		t.Fatalf("offer %q: selected %q, enabled %t; want %q", offer, got, pmd.IsEnabled(), want)
	}
	if pmd.configured != configured || want == "" && pmd.negotiated != (pmdParameters{}) {
		t.Fatalf("configuration/failed alternative leaked: configured %+v, negotiated %+v", pmd.configured, pmd.negotiated)
	}
}

func TestPMDDialDirectionalWindows(t *testing.T) {
	for _, tc := range []struct {
		name, selection string
		options         []PerMessageDeflateOption
		valid           bool
	}{
		{"default", "permessage-deflate", nil, true},
		{"declined", "", []PerMessageDeflateOption{WithServerMaxWindowBits(9), WithServerNoContextTakeover()}, true},
		{"unsolicited client 15", "permessage-deflate; client_max_window_bits=15", nil, false},
		{"unsolicited client 9", "permessage-deflate; client_max_window_bits=9", nil, false},
		{"offered client omitted", "permessage-deflate", []PerMessageDeflateOption{WithClientMaxWindowBits(15)}, true},
		{"offered client 15", "permessage-deflate; client_max_window_bits=15", []PerMessageDeflateOption{WithClientMaxWindowBits(15)}, true},
		{"unsupported client 9", "permessage-deflate; client_max_window_bits=9", []PerMessageDeflateOption{WithClientMaxWindowBits(15)}, false},
		{"bare response client", "permessage-deflate; client_max_window_bits", []PerMessageDeflateOption{WithClientMaxWindowBits(15)}, false},
		{"missing server 15", "permessage-deflate", []PerMessageDeflateOption{WithServerMaxWindowBits(15)}, false},
		{"missing server 9", "permessage-deflate", []PerMessageDeflateOption{WithServerMaxWindowBits(9)}, false},
		{"exceeded server 9", "permessage-deflate; server_max_window_bits=15", []PerMessageDeflateOption{WithServerMaxWindowBits(9)}, false},
		{"equal server 9", "permessage-deflate; server_max_window_bits=9", []PerMessageDeflateOption{WithServerMaxWindowBits(9)}, true},
		{"smaller server 8", "permessage-deflate; server_max_window_bits=8", []PerMessageDeflateOption{WithServerMaxWindowBits(9)}, true},
		{"quoted server", `permessage-deflate; server_max_window_bits="1\4"`, []PerMessageDeflateOption{WithServerMaxWindowBits(15)}, true},
		{"mandatory server flag missing", "permessage-deflate", []PerMessageDeflateOption{WithServerNoContextTakeover()}, false},
		{"mandatory server flag present", "permessage-deflate; server_no_context_takeover", []PerMessageDeflateOption{WithServerNoContextTakeover()}, true},
		{"optional client flag missing", "permessage-deflate", []PerMessageDeflateOption{WithClientNoContextTakeover()}, true},
		{"unsolicited flags", "permessage-deflate; client_no_context_takeover; server_no_context_takeover", nil, true},
		{"duplicate window", "permessage-deflate; server_max_window_bits=9; server_max_window_bits=15", nil, false},
		{"duplicate selection", "permessage-deflate, permessage-deflate", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pmd := NewPerMessageDeflateExtension(tc.options...).(*perMessageDeflate)
			wantOffer := pmd.Offer()
			configured, offered := pmd.configured, pmd.offered
			closed := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Sec-WebSocket-Extensions") != wantOffer {
					t.Errorf("offer changed: %q", r.Header.Get("Sec-WebSocket-Extensions"))
				}
				raw, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					closed <- err
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(3 * time.Second))
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
				if tc.selection != "" {
					fmt.Fprintf(rw, "Sec-WebSocket-Extensions: %s\r\n", tc.selection)
				}
				rw.WriteString("\r\n")
				rw.Flush()
				var b [1]byte
				_, err = rw.Read(b[:])
				closed <- err
			}))
			defer srv.Close()
			c, response, err := Dial(t.Context(), "ws"+srv.URL[4:], WithExtensions(pmd))
			if c != nil {
				c.conn.Close()
			}
			if (err == nil) != tc.valid || response == nil || err != nil && !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("Dial = %v, %v, %v", c, response, err)
			}
			if pmd.IsEnabled() != (tc.valid && tc.selection != "") || pmd.configured != configured || pmd.offered != offered {
				t.Fatalf("invalid committed state: %+v", pmd.negotiated)
			}
			if (!tc.valid || tc.selection == "") && pmd.negotiated != (pmdParameters{}) {
				t.Fatalf("failed/absent selection leaked: %+v", pmd.negotiated)
			}
			if err := <-closed; !errors.Is(err, io.EOF) {
				t.Fatalf("transport not closed cleanly: %v", err)
			}
		})
	}
}

func TestPMDLocalWindowRejectedBeforeTransport(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connects atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			connects.Add(1)
			c.Close()
		}
	}()
	defer func() { l.Close(); <-done }()
	for bits := 8; bits < 15; bits++ {
		pmd := NewPerMessageDeflateExtension(WithClientMaxWindowBits(bits))
		c, resp, err := Dial(t.Context(), "ws://"+l.Addr().String(), WithExtensions(pmd))
		if c != nil || resp != nil || !errors.Is(err, ErrInvalidExtension) || !errors.Is(err, ErrInvalidHandshakeHeader) || connects.Load() != 0 {
			t.Fatalf("client window %d: %v, %v, %v; connects %d", bits, c, resp, err, connects.Load())
		}
		pmd = NewPerMessageDeflateExtension(WithServerMaxWindowBits(bits))
		w := &countingHijacker{ResponseRecorder: httptest.NewRecorder()}
		r := upgradeRequest()
		r.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate")
		c, err = Upgrade(w, r, WithUpgradeExtensions(pmd))
		if c != nil || !errors.Is(err, ErrInvalidExtension) || !errors.Is(err, ErrInvalidHandshakeHeader) || w.calls != 0 || w.Body.Len() != 0 {
			t.Fatalf("server window %d: %v, %v; hijacks %d", bits, c, err, w.calls)
		}
	}
}

func TestPMDOfferAndResponseState(t *testing.T) {
	p := NewPerMessageDeflateExtension(WithServerMaxWindowBits(9), WithClientNoContextTakeover()).(*perMessageDeflate)
	offer := p.Offer()
	// A future option mutation must not change the already-sent constraint.
	p.configured.serverMaxWindowBits = 15
	if err := p.Negotiate("permessage-deflate; server_max_window_bits=15"); !errors.Is(err, ErrInvalidExtension) || p.IsEnabled() {
		t.Fatalf("lost actual offer %q: %v", offer, err)
	}
	if err := p.Negotiate("permessage-deflate; server_max_window_bits=8"); err != nil {
		t.Fatal(err)
	}
	if p.negotiated.clientNoContextTakeover || p.Offer() != "permessage-deflate; server_max_window_bits=8" || p.offered.serverMaxWindowBits != 9 {
		t.Fatal("configuration/offer leaked into agreed response")
	}
	if err := p.Negotiate("permessage-deflate; unknown"); err == nil || p.IsEnabled() || p.negotiated != (pmdParameters{}) {
		t.Fatal("failed response retained negotiated state")
	}
	for _, bits := range []int{-1, 0, 7, 16, 100} {
		p := NewPerMessageDeflateExtension(WithClientMaxWindowBits(bits), WithServerMaxWindowBits(bits))
		if p.Offer() != "permessage-deflate" {
			t.Fatalf("legacy invalid-option behavior changed for %d", bits)
		}
	}
}

func TestPMDClientHintIsNotResponseBound(t *testing.T) {
	for _, hint := range []int{-1, 8, 9, 14, 15} {
		offer := pmdParameters{clientMaxWindowBits: hint}
		for _, selection := range []int{0, 8, 9, 14, 15} {
			if err := validatePMDResponse(pmdParameters{clientMaxWindowBits: selection}, offer); err != nil {
				t.Fatalf("hint %d wrongly constrained response %d: %v", hint, selection, err)
			}
		}
	}
}

func TestPMDClientSelectedWindowCapability(t *testing.T) {
	for bits := 8; bits <= 15; bits++ {
		p := NewPerMessageDeflateExtension(WithClientMaxWindowBits(15))
		err := p.Negotiate(fmt.Sprintf("permessage-deflate; client_max_window_bits=%d", bits))
		if (err == nil) != (bits == 15) || p.IsEnabled() != (bits == 15) {
			t.Fatalf("selected local window %d: enabled %t, error %v", bits, p.IsEnabled(), err)
		}
	}
}

func TestPMDMalformedParametersInBothRoles(t *testing.T) {
	for _, name := range []string{"client_max_window_bits", "server_max_window_bits"} {
		for _, value := range []string{"0", "7", "16", "09", "+9", "9.0", "99999999999999999999999999"} {
			header := "permessage-deflate; " + name + "=" + value
			p := NewPerMessageDeflateExtension(WithClientMaxWindowBits(15))
			if err := p.Negotiate(header); !errors.Is(err, ErrInvalidExtension) || p.IsEnabled() {
				t.Fatalf("invalid client selection %q: %v", header, err)
			}
			checkPMDUpgrade(t, header, "")
			checkPMDUpgrade(t, header+", permessage-deflate", "permessage-deflate")
		}
		duplicate := "permessage-deflate; " + name + "=15; " + name + "=15"
		checkPMDUpgrade(t, duplicate, "")
		checkPMDUpgrade(t, duplicate+", permessage-deflate", "permessage-deflate")
	}
}

func TestPMDNoContextDirections(t *testing.T) {
	for _, server := range []bool{false, true} {
		for flags := 0; flags < 4; flags++ {
			params := pmdParameters{clientNoContextTakeover: flags&1 != 0, serverNoContextTakeover: flags&2 != 0}
			p := NewPerMessageDeflateExtension().(*perMessageDeflate)
			extensions, _ := parseExtensions([]string{params.String()})
			if err := p.negotiate(extensions, server); err != nil {
				t.Fatal(err)
			}
			local, peer := params.clientNoContextTakeover, params.serverNoContextTakeover
			if server {
				local, peer = peer, local
			}
			if p.localNoContextTakeover() != local || p.peerNoContextTakeover() != peer {
				t.Fatalf("wrong direction: server %t, params %+v", server, params)
			}
		}
	}
}

func TestPMDSmallerPeerWindows(t *testing.T) {
	// RFC 7692 section 7.2.3.1's Hello fits even the 8-bit window. The longer
	// independent vector was made with Python zlib 1.3.2, compressobj(wbits=-9),
	// Z_SYNC_FLUSH, then removal of the four-byte PMD suffix.
	compressed, err := hex.DecodeString("ecc9b10d80300c00b057c2ce217d03890c95024530f47df882c58b17b7ac1a6b3cc75695775cf931fbb98fb94473ce39e79c73ce39e79c73ce39e79c73ce39e79cfbe15e00")
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range []bool{false, true} {
		for bits := 8; bits <= 15; bits++ {
			p := NewPerMessageDeflateExtension().(*perMessageDeflate)
			name := "server_max_window_bits"
			if server {
				name = "client_max_window_bits"
			}
			extensions, _ := parseExtensions([]string{fmt.Sprintf("permessage-deflate; %s=%d", name, bits)})
			if err := p.negotiate(extensions, server); err != nil || !p.IsEnabled() {
				t.Fatalf("smaller peer window rejected: server %t, bits %d: %v", server, bits, err)
			}
			payload, want := compressed, bytes.Repeat([]byte("Hello, smaller peer window! "), 300)
			if bits == 8 {
				payload, want = []byte{0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00}, []byte("Hello")
			}
			f := &Frame{Final: true, Opcode: TextMessage, Rsv1: true, Payload: bytes.Clone(payload)}
			if err := p.ProcessIncomingFrame(f); err != nil || f.Rsv1 || !bytes.Equal(f.Payload, want) {
				t.Fatalf("peer window %d, server %t: %v, decoded %d bytes", bits, server, err, len(f.Payload))
			}
		}
	}
}

func TestPMDDirectConnLocalWindowGuard(t *testing.T) {
	p := NewPerMessageDeflateExtension()
	if err := p.Negotiate("permessage-deflate; server_max_window_bits=9"); err != nil {
		t.Fatal(err)
	}
	// Direct Negotiate is the client response path. If subsequently used as a
	// server, NewConn must bind that role and cannot emit a false 9-bit promise.
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := NewConn(raw, true, []Extension{p})
	defer c.conn.Close()
	if err := p.ProcessOutgoingFrame(&Frame{Final: true, Opcode: BinaryMessage, Payload: []byte("data")}); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("direct transform error = %v", err)
	}
	if err := c.WriteMessage(BinaryMessage, []byte("data")); err == nil || raw.written.Len() != 0 {
		t.Fatalf("unsupported direct send = %v, wire %x", err, raw.written.Bytes())
	}
}

func TestPMDDirectServerNegotiation(t *testing.T) {
	p := NewPerMessageDeflateExtension(WithClientMaxWindowBits(9), WithClientNoContextTakeover())
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := NewConn(raw, true, []Extension{p})
	defer c.conn.Close()
	if err := p.Negotiate("permessage-deflate; client_max_window_bits"); err != nil || !p.IsEnabled() {
		t.Fatalf("direct server peer-window negotiation = %v, enabled %t", err, p.IsEnabled())
	}
	if got := p.Offer(); got != "permessage-deflate; client_no_context_takeover; client_max_window_bits=9" {
		t.Fatalf("direct server selected %q", got)
	}
	pmd := p.(*perMessageDeflate)
	if !pmd.server || pmd.localNoContextTakeover() || !pmd.peerNoContextTakeover() {
		t.Fatal("Negotiate lost the bound server role")
	}
	if err := c.WriteMessage(BinaryMessage, []byte("hello")); err != nil || raw.written.Len() == 0 {
		t.Fatalf("direct server write = %v", err)
	}
}

func TestPMDDirectServerUnsupportedParameters(t *testing.T) {
	for _, local := range []bool{false, true} {
		option := WithClientMaxWindowBits(9)
		if local {
			option = WithServerMaxWindowBits(9)
		}
		p := NewPerMessageDeflateExtension(option)
		raw := &memoryConn{Reader: bytes.NewReader(nil)}
		c := NewConn(raw, true, []Extension{p})
		err := p.Negotiate("permessage-deflate")
		c.conn.Close()
		if (err != nil) != local || p.IsEnabled() || raw.written.Len() != 0 {
			t.Fatalf("direct server local=%t: %v, enabled %t, wire %x", local, err, p.IsEnabled(), raw.written.Bytes())
		}
		if local && !errors.Is(err, ErrInvalidExtension) {
			t.Fatalf("unsupported local window cause = %v", err)
		}
	}
}

func TestPMDIndependentZlibWindow(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("independent window oracle requires Python 3 with its standard-library zlib module")
	}
	type vector struct {
		Name  string
		Bits  int
		Plain []byte
		Wire  []byte
	}
	var vectors []vector
	rng := rand.New(rand.NewPCG(1, 2))
	block := make([]byte, 2048)
	for i := range block {
		block[i] = byte(rng.Uint32())
	}
	plain := bytes.Repeat(block, 3) // Historical false 9-bit claim fails here.
	for _, server := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			for flags := 0; flags < 4; flags++ {
				var options []PerMessageDeflateOption
				params := pmdParameters{clientNoContextTakeover: flags&1 != 0, serverNoContextTakeover: flags&2 != 0}
				if explicit {
					if server {
						params.serverMaxWindowBits = 15
					} else {
						options = append(options, WithClientMaxWindowBits(15))
						params.clientMaxWindowBits = 15
					}
				}
				p := NewPerMessageDeflateExtension(options...).(*perMessageDeflate)
				if server {
					c := NewConn(&memoryConn{Reader: bytes.NewReader(nil)}, true, []Extension{p})
					defer c.conn.Close()
				}
				if err := p.Negotiate(params.String()); err != nil || !p.IsEnabled() {
					t.Fatal(err)
				}
				for repeat := 0; repeat < 2; repeat++ {
					f := &Frame{Final: true, Opcode: BinaryMessage, Payload: plain}
					if err := p.ProcessOutgoingFrame(f); err != nil || !f.Rsv1 {
						t.Fatal(err)
					}
					vectors = append(vectors, vector{fmt.Sprintf("server=%t explicit=%t flags=%d message=%d", server, explicit, flags, repeat), 15, plain, f.Payload})
				}
			}
		}
	}
	input, err := json.Marshal(vectors)
	if err != nil {
		t.Fatal(err)
	}
	// Small output buffers force zlib to retain only its configured history;
	// a single large output buffer can conceal an invalid distance/window.
	const oracle = `
import base64, json, sys, zlib
def decode(wire, bits):
    d = zlib.decompressobj(-bits)
    pending = wire + bytes.fromhex('0000ffff010000ffff')
    out = bytearray()
    while pending:
        out += d.decompress(pending, 257)
        pending = d.unconsumed_tail
    out += d.flush()
    assert d.eof and not d.unused_data
    return bytes(out)
vectors = json.load(sys.stdin)
for v in vectors:
    assert decode(base64.b64decode(v['Wire']), v['Bits']) == base64.b64decode(v['Plain']), v['Name']
# Check the oracle actually rejects the original false small-window promise.
try:
    decode(base64.b64decode(vectors[0]['Wire']), 9)
except zlib.error:
    pass
else:
    raise AssertionError('negative 9-bit control did not exercise a long distance')
print('zlib %s: %d independent 15-bit send vectors pass; false 9-bit control rejected' % (zlib.ZLIB_RUNTIME_VERSION, len(vectors)))
`
	cmd := exec.CommandContext(t.Context(), python, "-c", oracle)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("independent zlib oracle: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
