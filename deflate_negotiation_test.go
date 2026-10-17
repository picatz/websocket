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
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPMDUpgradeDirectionalWindows(t *testing.T) {
	for _, tc := range []struct {
		name, offer, want string
		options           []pmdOption
	}{
		{"default", "permessage-deflate", "permessage-deflate", nil},
		{"bare client", "permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=15", nil},
		{"server 15", "permessage-deflate; server_max_window_bits=15", "permessage-deflate; server_max_window_bits=15", nil},
		{"small server window first", "permessage-deflate; server_max_window_bits=9; client_no_context_takeover, permessage-deflate", "permessage-deflate; client_no_context_takeover; server_max_window_bits=9", nil},
		{"preference order", "permessage-deflate; server_max_window_bits=14, permessage-deflate; server_max_window_bits=15", "permessage-deflate; server_max_window_bits=14", nil},
		{"configured server bound", "permessage-deflate; server_max_window_bits=12", "permessage-deflate; server_max_window_bits=10", []pmdOption{withServerMaxWindowBits(10)}},
		{"unsolicited server bound", "permessage-deflate", "permessage-deflate; server_max_window_bits=10", []pmdOption{withServerMaxWindowBits(10)}},
		{"unoffered client bound", "permessage-deflate", "", []pmdOption{withClientMaxWindowBits(9)}},
		{"unoffered client 15 omitted", "permessage-deflate", "permessage-deflate", []pmdOption{withClientMaxWindowBits(15)}},
		{"unsolicited server 15", "permessage-deflate", "permessage-deflate; server_max_window_bits=15", []pmdOption{withServerMaxWindowBits(15)}},
		{"peer limit", "permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=9", []pmdOption{withClientMaxWindowBits(9)}},
		{"lower peer hint", "permessage-deflate; client_max_window_bits=8", "permessage-deflate; client_max_window_bits=8", []pmdOption{withClientMaxWindowBits(9)}},
		{"higher peer hint", "permessage-deflate; client_max_window_bits=14", "permessage-deflate; client_max_window_bits=9", []pmdOption{withClientMaxWindowBits(9)}},
		{"no union across offers", "permessage-deflate; server_max_window_bits=9, permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=9", []pmdOption{withClientMaxWindowBits(9)}},
		{"fallback permitting peer bound", "permessage-deflate, permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=9", []pmdOption{withClientMaxWindowBits(9)}},
		{"both flags unsolicited", "permessage-deflate", "permessage-deflate; client_no_context_takeover; server_no_context_takeover", []pmdOption{withClientNoContextTakeover(), withServerNoContextTakeover()}},
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
			checkPMDUpgrade(t, server, server)
		})
	}
}

func checkPMDUpgrade(t *testing.T, offer, want string, options ...pmdOption) {
	t.Helper()
	r := upgradeRequest()
	r.Header.Set("Sec-WebSocket-Extensions", offer)
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
	pmd := newPerMessageDeflate(options...).(*perMessageDeflate)
	configured := pmd.configured
	c, err := upgradeExtensions(w, r, &Upgrader{}, []frameExtension{pmd})
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
		options         []pmdOption
		valid           bool
	}{
		{"default", "permessage-deflate", nil, true},
		{"declined", "", []pmdOption{withServerMaxWindowBits(9), withServerNoContextTakeover()}, true},
		{"default bare offer allows client 15", "permessage-deflate; client_max_window_bits=15", nil, true},
		{"default bare offer allows client 8", "permessage-deflate; client_max_window_bits=8", nil, true},
		{"offered client omitted", "permessage-deflate", []pmdOption{withClientMaxWindowBits(15)}, true},
		{"offered client 15", "permessage-deflate; client_max_window_bits=15", []pmdOption{withClientMaxWindowBits(15)}, true},
		{"smaller client 9", "permessage-deflate; client_max_window_bits=9", []pmdOption{withClientMaxWindowBits(15)}, true},
		{"bare response client", "permessage-deflate; client_max_window_bits", []pmdOption{withClientMaxWindowBits(15)}, false},
		{"missing server 15", "permessage-deflate", []pmdOption{withServerMaxWindowBits(15)}, false},
		{"missing server 9", "permessage-deflate", []pmdOption{withServerMaxWindowBits(9)}, false},
		{"exceeded server 9", "permessage-deflate; server_max_window_bits=15", []pmdOption{withServerMaxWindowBits(9)}, false},
		{"equal server 9", "permessage-deflate; server_max_window_bits=9", []pmdOption{withServerMaxWindowBits(9)}, true},
		{"smaller server 8", "permessage-deflate; server_max_window_bits=8", []pmdOption{withServerMaxWindowBits(9)}, true},
		{"quoted server", `permessage-deflate; server_max_window_bits="1\4"`, []pmdOption{withServerMaxWindowBits(15)}, true},
		{"mandatory server flag missing", "permessage-deflate", []pmdOption{withServerNoContextTakeover()}, false},
		{"mandatory server flag present", "permessage-deflate; server_no_context_takeover", []pmdOption{withServerNoContextTakeover()}, true},
		{"optional client flag missing", "permessage-deflate", []pmdOption{withClientNoContextTakeover()}, true},
		{"unsolicited flags", "permessage-deflate; client_no_context_takeover; server_no_context_takeover", nil, true},
		{"duplicate window", "permessage-deflate; server_max_window_bits=9; server_max_window_bits=15", nil, false},
		{"duplicate selection", "permessage-deflate, permessage-deflate", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pmd := newPerMessageDeflate(tc.options...).(*perMessageDeflate)
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
			c, response, err := dialExtensions(t.Context(), &Dialer{}, "ws"+srv.URL[4:], []frameExtension{pmd})
			if c != nil {
				c.conn.Close()
			}
			if (err == nil) != tc.valid || response == nil || err != nil && !errors.Is(err, errInvalidExtension) {
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

// TestPMDSmallLocalWindowsRoundTrip negotiates every send window end to end,
// in both roles, and checks each compressed message stays inside its window.
func TestPMDSmallLocalWindowsRoundTrip(t *testing.T) {
	message := bytes.Repeat([]byte("window bounded permessage-deflate "), 2000)
	for bits := 8; bits <= 15; bits++ {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			server := newPerMessageDeflate(withServerMaxWindowBits(bits))
			client := newPerMessageDeflate(withClientMaxWindowBits(bits))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := upgradeExtensions(w, r, &Upgrader{}, []frameExtension{server})
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				for {
					kind, data, err := c.ReadMessage()
					if err != nil {
						return
					}
					if err := c.WriteMessage(kind, data); err != nil {
						t.Error(err)
						return
					}
				}
			}))
			defer srv.Close()
			c, resp, err := dialExtensions(t.Context(), &Dialer{}, "ws"+srv.URL[4:], []frameExtension{client})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			want := fmt.Sprintf("permessage-deflate; client_max_window_bits=%d; server_max_window_bits=%d", bits, bits)
			if got := resp.Header.Get("Sec-WebSocket-Extensions"); got != want {
				t.Fatalf("selected %q, want %q", got, want)
			}
			for range 3 {
				if err := c.WriteMessage(BinaryMessage, message); err != nil {
					t.Fatal(err)
				}
				_, got, err := c.ReadMessage()
				if err != nil || !bytes.Equal(got, message) {
					t.Fatalf("echo = %d bytes, %v", len(got), err)
				}
			}
			for _, p := range []*perMessageDeflate{server.(*perMessageDeflate), client.(*perMessageDeflate)} {
				if got := p.localWindowBits(); got != bits {
					t.Fatalf("local window = %d, want %d", got, bits)
				}
			}
		})
	}
}

func TestPMDOfferAndResponseState(t *testing.T) {
	p := newPerMessageDeflate(withServerMaxWindowBits(9), withClientNoContextTakeover()).(*perMessageDeflate)
	offer := p.Offer()
	// A future option mutation must not change the already-sent constraint.
	p.configured.serverMaxWindowBits = 15
	if err := p.Negotiate("permessage-deflate; server_max_window_bits=15"); !errors.Is(err, errInvalidExtension) || p.IsEnabled() {
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
		p := newPerMessageDeflate(withClientMaxWindowBits(bits), withServerMaxWindowBits(bits))
		if p.Offer() != "permessage-deflate; client_max_window_bits" {
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
		p := newPerMessageDeflate(withClientMaxWindowBits(15))
		err := p.Negotiate(fmt.Sprintf("permessage-deflate; client_max_window_bits=%d", bits))
		if err != nil || !p.IsEnabled() || p.(*perMessageDeflate).localWindowBits() != bits {
			t.Fatalf("selected local window %d: enabled %t, error %v", bits, p.IsEnabled(), err)
		}
	}
}

func TestPMDMalformedParametersInBothRoles(t *testing.T) {
	for _, name := range []string{"client_max_window_bits", "server_max_window_bits"} {
		for _, value := range []string{"0", "7", "16", "09", "+9", "9.0", "99999999999999999999999999"} {
			header := "permessage-deflate; " + name + "=" + value
			p := newPerMessageDeflate(withClientMaxWindowBits(15))
			if err := p.Negotiate(header); !errors.Is(err, errInvalidExtension) || p.IsEnabled() {
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
			p := newPerMessageDeflate().(*perMessageDeflate)
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
			p := newPerMessageDeflate().(*perMessageDeflate)
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
			f := &wireFrame{Final: true, Opcode: opText, Rsv1: true, Payload: bytes.Clone(payload)}
			if err := p.ProcessIncomingFrame(f); err != nil || f.Rsv1 || !bytes.Equal(f.Payload, want) {
				t.Fatalf("peer window %d, server %t: %v, decoded %d bytes", bits, server, err, len(f.Payload))
			}
		}
	}
}

func TestPMDDirectConnRoleSwitchHonorsWindow(t *testing.T) {
	p := newPerMessageDeflate()
	if err := p.Negotiate("permessage-deflate; server_max_window_bits=9"); err != nil {
		t.Fatal(err)
	}
	// Direct Negotiate is the client response path. If subsequently used as a
	// server, NewConn binds that role and the 9-bit promise applies to sending.
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := newTestConn(raw, ConnConfig{Server: true}, []frameExtension{p}...)
	defer c.conn.Close()
	if got := p.(*perMessageDeflate).localWindowBits(); got != 9 {
		t.Fatalf("server send window = %d, want 9", got)
	}
	if err := c.WriteMessage(BinaryMessage, []byte("data")); err != nil || raw.written.Len() == 0 {
		t.Fatalf("direct send = %v, wire %x", err, raw.written.Bytes())
	}
}

func TestPMDDirectServerNegotiation(t *testing.T) {
	p := newPerMessageDeflate(withClientMaxWindowBits(9), withClientNoContextTakeover())
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := newTestConn(raw, ConnConfig{Server: true}, []frameExtension{p}...)
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

func TestPMDDirectServerWindowParameters(t *testing.T) {
	for _, local := range []bool{false, true} {
		option := withClientMaxWindowBits(9)
		if local {
			option = withServerMaxWindowBits(9)
		}
		p := newPerMessageDeflate(option)
		raw := &memoryConn{Reader: bytes.NewReader(nil)}
		c := newTestConn(raw, ConnConfig{Server: true}, []frameExtension{p}...)
		err := p.Negotiate("permessage-deflate")
		c.conn.Close()
		// A server may always bound its own window (RFC 7692 section 7.1.2.1),
		// but cannot bound the client's without an offered parameter.
		if err != nil || p.IsEnabled() != local || raw.written.Len() != 0 {
			t.Fatalf("direct server local=%t: %v, enabled %t, wire %x", local, err, p.IsEnabled(), raw.written.Bytes())
		}
		if local && p.Offer() != "permessage-deflate; server_max_window_bits=9" {
			t.Fatalf("local window response = %q", p.Offer())
		}
	}
}

func TestPMDIndependentZlibWindow(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("independent window oracle requires Python 3 with its standard-library zlib module")
	}
	type message struct{ Plain, Wire []byte }
	type stream struct {
		Name     string
		Bits     int
		Takeover bool
		Messages []message
	}
	var streams []stream
	rng := rand.New(rand.NewPCG(1, 2))
	block := make([]byte, 2048)
	for i := range block {
		block[i] = byte(rng.Uint32())
	}
	plain := bytes.Repeat(block, 3) // Distances of 2048 exceed windows below 12 bits.
	text := bytes.Repeat([]byte("context takeover reuses this sentence. "), 40)
	for bits := 8; bits <= 15; bits++ {
		for _, server := range []bool{false, true} {
			for flags := 0; flags < 4; flags++ {
				params := pmdParameters{clientNoContextTakeover: flags&1 != 0, serverNoContextTakeover: flags&2 != 0}
				var options []pmdOption
				if server {
					params.serverMaxWindowBits = bits
				} else {
					options = append(options, withClientMaxWindowBits(bits))
					params.clientMaxWindowBits = bits
				}
				p := newPerMessageDeflate(options...).(*perMessageDeflate)
				if server {
					c := newTestConn(&memoryConn{Reader: bytes.NewReader(nil)}, ConnConfig{Server: true}, []frameExtension{p}...)
					defer c.conn.Close()
				}
				if err := p.Negotiate(params.String()); err != nil || !p.IsEnabled() {
					t.Fatal(err)
				}
				st := stream{Name: fmt.Sprintf("bits=%d server=%t flags=%d", bits, server, flags), Bits: bits, Takeover: !p.localNoContextTakeover()}
				for _, m := range [][]byte{plain, text, text, plain, nil} {
					f := &wireFrame{Final: true, Opcode: opBinary, Payload: m}
					if err := p.ProcessOutgoingFrame(f); err != nil || !f.Rsv1 {
						t.Fatal(err)
					}
					st.Messages = append(st.Messages, message{m, f.Payload})
				}
				streams = append(streams, st)
			}
		}
	}
	input, err := json.Marshal(streams)
	if err != nil {
		t.Fatal(err)
	}
	// Small output buffers force zlib to retain only its configured history;
	// a single large output buffer can conceal an invalid distance/window.
	// With context takeover one decompressor spans the stream's messages
	// (RFC 7692 section 7.2.2); otherwise each message starts afresh.
	const oracle = `
import base64, json, sys, zlib
def decode(d, wire):
    pending = wire + bytes.fromhex('0000ffff')
    out = bytearray()
    while pending:
        out += d.decompress(pending, 257)
        pending = d.unconsumed_tail
    assert not d.eof and not d.unused_data
    return bytes(out)
streams = json.load(sys.stdin)
count = 0
for s in streams:
    d = zlib.decompressobj(-s['Bits'])
    for m in s['Messages']:
        if not s['Takeover']:
            d = zlib.decompressobj(-s['Bits'])
        assert decode(d, base64.b64decode(m['Wire'] or '')) == base64.b64decode(m['Plain'] or ''), s['Name']
        count += 1
# Check the oracle rejects a 15-bit stream decoded with a 9-bit window.
wide = [s for s in streams if s['Bits'] == 15][0]
try:
    decode(zlib.decompressobj(-9), base64.b64decode(wide['Messages'][0]['Wire']))
except zlib.error:
    pass
else:
    raise AssertionError('negative 9-bit control did not exercise a long distance')
print('zlib %s: %d messages in %d streams pass; false 9-bit control rejected' % (zlib.ZLIB_RUNTIME_VERSION, count, len(streams)))
`
	cmd := exec.CommandContext(t.Context(), python, "-c", oracle)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("independent zlib oracle: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
