package websocket

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pmdLink negotiates params for a client sender and a server receiver.
func pmdLink(t *testing.T, params string, options ...pmdOption) (send, recv *perMessageDeflate) {
	t.Helper()
	send = newPerMessageDeflate(options...).(*perMessageDeflate)
	if err := send.Negotiate(params); err != nil || !send.IsEnabled() {
		t.Fatalf("client Negotiate(%q) = %v", params, err)
	}
	recv = newPerMessageDeflate().(*perMessageDeflate)
	recv.server = true
	extensions, _ := parseExtensions([]string{params})
	recv.negotiated, recv.enabled = mustParsePMD(t, extensions[0].params), true
	return send, recv
}

func mustParsePMD(t *testing.T, params []extensionParameter) pmdParameters {
	t.Helper()
	p, err := parsePMDParameters(params, false)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sendWire writes messages through a client Conn and returns the raw wire.
func sendWire(t *testing.T, pmd *perMessageDeflate, messages ...[]byte) []byte {
	t.Helper()
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := newTestConn(raw, ConnConfig{}, []frameExtension{pmd}...)
	for _, m := range messages {
		if err := c.WriteMessage(BinaryMessage, m); err != nil {
			t.Fatal(err)
		}
	}
	return raw.written.Bytes()
}

// wireFrames parses raw frames without extensions, unmasking client frames.
func wireFrames(t *testing.T, wire []byte) []*wireFrame {
	t.Helper()
	c := frameConn(wire, true, 0)
	c.extensions = []frameExtension{&negotiationExtension{name: "inspect", enabled: true}}
	var frames []*wireFrame
	for {
		f := new(wireFrame)
		n, err := c.readFrameHeader(f)
		if err == io.EOF {
			return frames
		}
		if err != nil {
			t.Fatal(err)
		}
		if f.Masked {
			io.ReadFull(c.br, f.MaskKey[:])
		}
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(c.br, f.Payload); err != nil {
			t.Fatal(err)
		}
		if f.Masked {
			xor(f.MaskKey[:], f.Payload)
		}
		frames = append(frames, f)
	}
}

// readAll decodes every message on wire with a server Conn using pmd.
func readAll(t *testing.T, pmd *perMessageDeflate, wire []byte) [][]byte {
	t.Helper()
	c := newTestConn(&memoryConn{Reader: bytes.NewReader(wire)}, ConnConfig{Server: true}, []frameExtension{pmd}...)
	var out [][]byte
	for {
		_, data, err := c.ReadMessage()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, data)
	}
}

// TestPMDFragmentedSend covers RFC 7692 section 6: a compressed message may
// span frames, with RSV1 set only on the first.
func TestPMDFragmentedSend(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	big := make([]byte, 3*writeFragmentSize+123)
	for i := range big {
		big[i] = byte(r.Uint32()) // Incompressible, so it needs several frames.
	}
	send, recv := pmdLink(t, "permessage-deflate")
	wire := sendWire(t, send, big)
	frames := wireFrames(t, wire)
	if len(frames) < 4 {
		t.Fatalf("got %d frames, want at least 4", len(frames))
	}
	for i, f := range frames {
		wantOpcode := opContinuation
		if i == 0 {
			wantOpcode = opBinary
		}
		if f.Opcode != wantOpcode || f.Rsv1 != (i == 0) || f.Final != (i == len(frames)-1) {
			t.Fatalf("frame %d: opcode %v rsv1 %t fin %t", i, f.Opcode, f.Rsv1, f.Final)
		}
		if len(f.Payload) == 0 || len(f.Payload) > writeFragmentSize {
			t.Fatalf("frame %d payload %d bytes", i, len(f.Payload))
		}
	}
	got := readAll(t, recv, wire)
	if len(got) != 1 || !bytes.Equal(got[0], big) {
		t.Fatalf("decoded %d messages", len(got))
	}
}

// TestPMDContextTakeoverSend checks a repeated message compresses to a few
// bytes when history carries over (RFC 7692 section 7.2.1), and not otherwise.
func TestPMDContextTakeoverSend(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	msg := make([]byte, 4096)
	for i := range msg {
		msg[i] = byte('a' + r.IntN(26))
	}
	for _, tc := range []struct {
		params   string
		options  []pmdOption
		takeover bool
	}{
		{"permessage-deflate", nil, true},
		{"permessage-deflate; client_max_window_bits=13", nil, true},
		{"permessage-deflate; client_no_context_takeover", nil, false},
		{"permessage-deflate; client_max_window_bits=13; client_no_context_takeover", nil, false},
		{"permessage-deflate", []pmdOption{withCompressionLevel(1)}, true},
		{"permessage-deflate", []pmdOption{withCompressionLevel(9)}, true},
	} {
		t.Run(tc.params, func(t *testing.T) {
			send, recv := pmdLink(t, tc.params, tc.options...)
			wire := sendWire(t, send, msg, msg, msg)
			frames := wireFrames(t, wire)
			if len(frames) != 3 {
				t.Fatalf("got %d frames", len(frames))
			}
			first, second := len(frames[0].Payload), len(frames[1].Payload)
			if tc.takeover != (second < first/10) {
				t.Fatalf("takeover %t: first %d bytes, second %d bytes", tc.takeover, first, second)
			}
			for i, got := range readAll(t, recv, wire) {
				if !bytes.Equal(got, msg) {
					t.Fatalf("message %d mismatch", i)
				}
			}
		})
	}
}

// TestPMDEmptyCompressedMessage follows RFC 7692 section 7.2.3.6.
func TestPMDEmptyCompressedMessage(t *testing.T) {
	send, recv := pmdLink(t, "permessage-deflate")
	wire := sendWire(t, send, nil, []byte("x"), nil)
	frames := wireFrames(t, wire)
	if len(frames) != 3 || !bytes.Equal(frames[0].Payload, []byte{0}) || !frames[0].Rsv1 {
		t.Fatalf("empty message frame = %+v", frames[0])
	}
	got := readAll(t, recv, wire)
	if len(got) != 3 || len(got[0]) != 0 || string(got[1]) != "x" || len(got[2]) != 0 {
		t.Fatalf("decoded %q", got)
	}
}

// TestPMDCompressionThreshold sends short messages uncompressed and keeps
// them out of the compression context (RFC 7692 section 6).
func TestPMDCompressionThreshold(t *testing.T) {
	send, recv := pmdLink(t, "permessage-deflate", withCompressionThreshold(64))
	short := []byte("short")
	long := bytes.Repeat([]byte("long enough to compress "), 10)
	wire := sendWire(t, send, short, long, short, long)
	frames := wireFrames(t, wire)
	for i, f := range frames {
		if f.Rsv1 != (i%2 == 1) {
			t.Fatalf("frame %d rsv1 = %t", i, f.Rsv1)
		}
	}
	if !bytes.Equal(frames[0].Payload, short) {
		t.Fatalf("uncompressed payload = %q", frames[0].Payload)
	}
	got := readAll(t, recv, wire)
	if len(got) != 4 || !bytes.Equal(got[0], short) || !bytes.Equal(got[3], long) {
		t.Fatalf("decoded %q", got)
	}
}

// TestPMDSendRetentionAndPooling checks which state survives a message.
func TestPMDSendRetentionAndPooling(t *testing.T) {
	unlimited := withCompressionMemoryLimit(0)
	for _, tc := range []struct {
		params    string
		limit     pmdOption
		retention sendRetention
	}{
		{"permessage-deflate", nil, retainHistory},
		{"permessage-deflate; client_max_window_bits=10", nil, retainHistory},
		{"permessage-deflate", unlimited, retainCompressor},
		{"permessage-deflate; client_max_window_bits=10", unlimited, retainCompressor},
		{"permessage-deflate; client_no_context_takeover", nil, retainNothing},
		{"permessage-deflate; client_no_context_takeover", unlimited, retainNothing},
		{"permessage-deflate; client_max_window_bits=10; client_no_context_takeover", nil, retainNothing},
	} {
		var options []pmdOption
		if tc.limit != nil {
			options = append(options, tc.limit)
		}
		send, _ := pmdLink(t, tc.params, options...)
		msg := []byte("hello hello hello")
		sendWire(t, send, msg)
		s := &send.send
		if s.retention != tc.retention || s.active {
			t.Fatalf("%s: retention %d active %t", tc.params, s.retention, s.active)
		}
		held := s.flate != nil || s.enc != nil
		if held != (tc.retention == retainCompressor) {
			t.Fatalf("%s: held compressor %t", tc.params, held)
		}
		if wantHistory := tc.retention == retainHistory; wantHistory != bytes.Equal(s.history, msg) {
			t.Fatalf("%s: history %q", tc.params, s.history)
		}
	}
}

func TestPMDSendWindowsAndLevels(t *testing.T) {
	msg := bytes.Repeat([]byte("abcdefghij0123456789"), 5000)
	for bits := 8; bits <= 15; bits++ {
		for _, level := range []int{-2, 0, 1, 5, 6, 9} {
			t.Run(fmt.Sprintf("w%d/l%d", bits, level), func(t *testing.T) {
				send, recv := pmdLink(t, fmt.Sprintf("permessage-deflate; client_max_window_bits=%d", bits), withCompressionLevel(level))
				wire := sendWire(t, send, msg, msg[:1000], msg)
				got := readAll(t, recv, wire)
				if len(got) != 3 || !bytes.Equal(got[0], msg) || !bytes.Equal(got[1], msg[:1000]) || !bytes.Equal(got[2], msg) {
					t.Fatal("round trip mismatch")
				}
			})
		}
	}
}

func TestPMDInvalidOptionValuesIgnored(t *testing.T) {
	p := newPerMessageDeflate(withCompressionLevel(10), withCompressionLevel(-3), withCompressionThreshold(-1)).(*perMessageDeflate)
	if p.level != -1 || p.threshold != 0 {
		t.Fatalf("level %d threshold %d", p.level, p.threshold)
	}
}

func TestPMDSendAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under the race detector")
	}
	send, _ := pmdLink(t, "permessage-deflate")
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := newTestConn(raw, ConnConfig{}, []frameExtension{send}...)
	msg := bytes.Repeat([]byte("steady state message "), 100)
	c.WriteMessage(BinaryMessage, msg)
	allocs := testing.AllocsPerRun(100, func() {
		raw.written.Reset()
		if err := c.WriteMessage(BinaryMessage, msg); err != nil {
			t.Fatal(err)
		}
	})
	// The message writer, its frame and the frame's extension view; the
	// compressor and the encoded bytes reuse connection buffers.
	if allocs > 3 {
		t.Fatalf("allocations per compressed message = %v", allocs)
	}
}

// TestEncoderPoolReuse checks that a message encoded without context takeover
// returns its bounded-window encoder to the pool it is taken from, at every
// level including the default.
func TestEncoderPoolReuse(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under the race detector")
	}
	msg := []byte("hello hello hello")
	for _, level := range []int{flate.DefaultCompression, flate.HuffmanOnly, flate.NoCompression, flate.BestSpeed, 6, flate.BestCompression} {
		s := &deflateSender{bits: 10, level: level}
		var dst []byte
		allocs := testing.AllocsPerRun(20, func() {
			s.begin()
			dst = s.end(s.write(dst[:0], msg))
		})
		if allocs > 0 {
			t.Errorf("level %d: %v allocations per message; the encoder pool is missing", level, allocs)
		}
	}
}

func TestPMDMemoryLimitNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		offer string
	}{
		{"unlimited offer", 0, "permessage-deflate; client_max_window_bits"},
		{"large limit keeps windows", 1 << 20, "permessage-deflate; client_max_window_bits"},
		{"64 KiB fits 15 bits", 64 << 10, "permessage-deflate; client_max_window_bits"},
		{"16 KiB", 16 << 10, "permessage-deflate; client_max_window_bits=13; server_max_window_bits=13"},
		{"1000 bytes", 1000, "permessage-deflate; client_max_window_bits=8; server_max_window_bits=8"},
		{"too small for any window", 300, "permessage-deflate; client_no_context_takeover; server_no_context_takeover; client_max_window_bits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPerMessageDeflate(withCompressionMemoryLimit(tc.limit))
			if got := p.Offer(); got != tc.offer {
				t.Fatalf("offer = %q, want %q", got, tc.offer)
			}
		})
	}
	for _, tc := range []struct {
		name, offer, want string
		limit             int
	}{
		{"bounds both windows", "permessage-deflate; client_max_window_bits", "permessage-deflate; client_max_window_bits=11; server_max_window_bits=11", 4 << 10},
		{"refuses unbounded client context", "permessage-deflate", "permessage-deflate; client_no_context_takeover; server_max_window_bits=11", 4 << 10},
		{"keeps smaller offered windows", "permessage-deflate; client_max_window_bits=9; server_max_window_bits=9", "permessage-deflate; client_max_window_bits=9; server_max_window_bits=9", 4 << 10},
		{"no context when nothing fits", "permessage-deflate; client_max_window_bits", "permessage-deflate; client_no_context_takeover; server_no_context_takeover; client_max_window_bits=15", 100},
		{"large limit changes nothing", "permessage-deflate", "permessage-deflate", 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkPMDUpgrade(t, tc.offer, tc.want, withCompressionMemoryLimit(tc.limit))
		})
	}
}

// TestPMDMemoryLimitRetention checks the state kept between messages fits
// the limit end to end, in both directions and both roles.
func TestPMDMemoryLimitRetention(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	msg := make([]byte, 100_000)
	for i := range msg {
		msg[i] = byte('a' + r.IntN(8))
	}
	for _, limit := range []int{100, 1000, 4 << 10, 64 << 10} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			server := newPerMessageDeflate(withCompressionMemoryLimit(limit)).(*perMessageDeflate)
			client := newPerMessageDeflate(withCompressionMemoryLimit(limit)).(*perMessageDeflate)
			done := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				c, err := upgradeExtensions(w, r, &Upgrader{MaxMessageSize: -1}, []frameExtension{server})
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
			c, _, err := dialExtensions(t.Context(), &Dialer{MaxMessageSize: -1}, "ws"+srv.URL[4:], []frameExtension{client})
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := c.WriteMessage(BinaryMessage, msg); err != nil {
					t.Fatal(err)
				}
				if _, got, err := c.ReadMessage(); err != nil || !bytes.Equal(got, msg) {
					t.Fatalf("echo: %v", err)
				}
			}
			c.Close()
			<-done
			for name, p := range map[string]*perMessageDeflate{"server": server, "client": client} {
				s := &p.send
				if s.flate != nil || s.enc != nil {
					t.Fatalf("%s retained a compressor under a limit", name)
				}
				retained := cap(s.history)
				if p.receive != nil {
					retained += cap(p.receive.history)
				}
				if retained > limit {
					t.Fatalf("%s retained %d bytes, limit %d", name, retained, limit)
				}
			}
		})
	}
}

// xorExtension is a frame extension that obfuscates data payloads and marks
// them with RSV2, so composition with compression is observable on the wire.
type xorExtension struct {
	enabled       bool
	in, out       int
	tamperRSV1    bool
	sawCompressed bool
}

func (*xorExtension) Name() string      { return "x-xor" }
func (*xorExtension) Offer() string     { return "x-xor" }
func (e *xorExtension) IsEnabled() bool { return e.enabled }
func (e *xorExtension) Negotiate(header string) error {
	e.enabled = strings.Contains(header, "x-xor")
	return nil
}
func (e *xorExtension) ProcessOutgoingFrame(f *wireFrame) error {
	if f.Opcode >= opClose {
		return nil
	}
	e.out++
	if e.tamperRSV1 {
		f.Rsv1 = !f.Rsv1
	}
	p := bytes.Clone(f.Payload)
	for i := range p {
		p[i] ^= 0x5a
	}
	f.Payload, f.Rsv2 = p, true
	return nil
}
func (e *xorExtension) ProcessIncomingFrame(f *wireFrame) error {
	if f.Opcode >= opClose {
		return nil
	}
	if !f.Rsv2 {
		return errors.New("missing RSV2 mark")
	}
	e.in++
	e.sawCompressed = e.sawCompressed || f.Rsv1
	for i := range f.Payload {
		f.Payload[i] ^= 0x5a
	}
	f.Rsv2 = false
	return nil
}

// TestPMDComposesWithFrameExtensions runs compression inside a custom frame
// extension in both directions: compress, then transform each frame on send;
// transform each frame, then decompress on receive.
func TestPMDComposesWithFrameExtensions(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	big := make([]byte, 2*writeFragmentSize+77)
	for i := range big {
		big[i] = byte(r.Uint32())
	}
	text := bytes.Repeat([]byte("composed "), 1000)
	serverXor := &xorExtension{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgradeExtensions(w, r, &Upgrader{MaxMessageSize: -1}, []frameExtension{newPerMessageDeflate(), serverXor})
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
	clientXor := &xorExtension{}
	c, resp, err := dialExtensions(t.Context(), &Dialer{MaxMessageSize: -1}, "ws"+srv.URL[4:], []frameExtension{newPerMessageDeflate(), clientXor})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := resp.Header.Get("Sec-WebSocket-Extensions"); got != "permessage-deflate; client_max_window_bits=15, x-xor" {
		t.Fatalf("selection %q", got)
	}
	for _, m := range [][]byte{text, big, text, nil} {
		if err := c.WriteMessage(BinaryMessage, m); err != nil {
			t.Fatal(err)
		}
		if _, got, err := c.ReadMessage(); err != nil || !bytes.Equal(got, m) {
			t.Fatalf("echo %d bytes: got %d, %v", len(m), len(got), err)
		}
	}
	if !clientXor.sawCompressed || clientXor.in < 6 || clientXor.out < 6 {
		t.Fatalf("custom extension saw in=%d out=%d compressed=%t", clientXor.in, clientXor.out, clientXor.sawCompressed)
	}
}

func TestPMDCompositionRejectsRSV1Changes(t *testing.T) {
	pmd := newPerMessageDeflate()
	if err := pmd.Negotiate("permessage-deflate"); err != nil {
		t.Fatal(err)
	}
	custom := &xorExtension{enabled: true, tamperRSV1: true}
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	c := newTestConn(raw, ConnConfig{}, []frameExtension{pmd, custom}...)
	err := c.WriteMessage(BinaryMessage, []byte("hello"))
	if !errors.Is(err, errUnsupportedExtensionComposition) || raw.written.Len() != 0 {
		t.Fatalf("RSV1 change = %v, wire %x", err, raw.written.Bytes())
	}
	// Compression consumed input into the takeover context: writes stay poisoned.
	if err := c.WriteMessage(BinaryMessage, []byte("again")); !errors.Is(err, ErrWriteFailed) {
		t.Fatalf("later write = %v", err)
	}
}

func TestPMDCloseReleasesSendCompressor(t *testing.T) {
	for _, limit := range []int{0, DefaultCompressionMemoryLimit} {
		send, _ := pmdLink(t, "permessage-deflate", withCompressionMemoryLimit(limit))
		raw := &memoryConn{Reader: bytes.NewReader(nil)}
		c := newTestConn(raw, ConnConfig{}, []frameExtension{send}...)
		if err := c.WriteMessage(BinaryMessage, []byte("hello hello")); err != nil {
			t.Fatal(err)
		}
		if limit == 0 && send.send.flate == nil || limit > 0 && send.send.history == nil {
			t.Fatalf("limit %d: takeover state not retained between messages", limit)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if send.send.flate != nil || send.send.enc != nil || send.send.history != nil {
			t.Fatalf("limit %d: Close kept send compression state", limit)
		}
	}
}

// BenchmarkDeflateSenderRetention compares the per-message cost of the
// takeover retention modes at the default level and a 15-bit window.
func BenchmarkDeflateSenderRetention(b *testing.B) {
	r := rand.New(rand.NewPCG(7, 8))
	vocab := strings.Fields(`{"id": "name": "value": true, false, null, 42, "event", "user", "message", "created_at": "2026-10-17T00:00:00Z"}`)
	var corpus []byte
	for len(corpus) < 1<<20 {
		corpus = append(corpus, vocab[r.IntN(len(vocab))]...)
		corpus = append(corpus, ' ')
	}
	for _, size := range []int{256, 4 << 10, 64 << 10} {
		for _, mode := range []struct {
			name      string
			retention sendRetention
		}{{"compressor", retainCompressor}, {"history", retainHistory}, {"nothing", retainNothing}} {
			b.Run(fmt.Sprintf("%d/%s", size, mode.name), func(b *testing.B) {
				s := &deflateSender{bits: 15, level: flate.DefaultCompression, retention: mode.retention}
				var dst []byte
				off, n := 0, 0
				b.SetBytes(int64(size))
				for b.Loop() {
					if off+size > len(corpus) {
						off = 0
					}
					s.begin()
					dst = s.end(s.write(dst[:0], corpus[off:off+size]))
					off += size
					n += len(dst)
				}
				b.ReportMetric(float64(n)/float64(b.N*size), "ratio")
				s.release()
			})
		}
	}
}
