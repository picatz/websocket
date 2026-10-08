package websocket

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// entryExtension makes negotiation state and frame processing independently
// observable. An extension's name alone is not a composition capability.
type entryExtension struct {
	name                             string
	enabled, decline                 bool
	negotiations, incoming, outgoing int
}

func (e *entryExtension) Name() string    { return e.name }
func (e *entryExtension) Offer() string   { return e.name }
func (e *entryExtension) IsEnabled() bool { return e.enabled }
func (e *entryExtension) Negotiate(header string) error {
	e.negotiations++
	e.enabled = false
	for _, selection := range strings.Split(header, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(selection), ";")
		if name == e.name && !e.decline {
			e.enabled = true
		}
	}
	return nil
}
func (e *entryExtension) ProcessIncomingFrame(*Frame) error { e.incoming++; return nil }
func (e *entryExtension) ProcessOutgoingFrame(*Frame) error { e.outgoing++; return nil }

type entryConn struct {
	*memoryConn
	reads, writes, closes int
}

func newEntryConn(wire []byte) *entryConn {
	return &entryConn{memoryConn: &memoryConn{Reader: bytes.NewReader(wire)}}
}
func (c *entryConn) Read(p []byte) (int, error)  { c.reads++; return c.Reader.Read(p) }
func (c *entryConn) Write(p []byte) (int, error) { c.writes++; return c.memoryConn.Write(p) }
func (c *entryConn) Close() error                { c.closes++; return nil }

type entryHijacker struct {
	*httptest.ResponseRecorder
	raw   *entryConn
	calls int
}

func (w *entryHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	return w.raw, bufio.NewReadWriter(bufio.NewReader(w.raw), bufio.NewWriter(w.raw)), nil
}

// Exercise ResponseController's Unwrap route, introduced independently of PMD.
type entryUnwrapper struct{ http.ResponseWriter }

func (w *entryUnwrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestPMDEntryUpgradeComposition(t *testing.T) {
	for _, tc := range []struct {
		name, offer                string
		decline, duplicate, reject bool
		wantSelection              string
	}{
		{name: "PMD only selected", offer: "permessage-deflate", wantSelection: "permessage-deflate"},
		{name: "custom only selected", offer: "entry-custom", wantSelection: "entry-custom"},
		{name: "neither selected", offer: "unknown"},
		{name: "custom declines combined offer", offer: "permessage-deflate, entry-custom", decline: true, wantSelection: "permessage-deflate"},
		{name: "PMD first mixed", offer: "permessage-deflate, entry-custom", reject: true},
		{name: "custom first mixed", offer: "entry-custom, permessage-deflate", reject: true},
		{name: "multiple PMD owners", offer: "permessage-deflate", duplicate: true, reject: true},
		{name: "multiple disabled PMD owners", offer: "entry-custom", duplicate: true, wantSelection: "entry-custom"},
	} {
		for _, wrapped := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/wrapped=%t/reversed=%t", tc.name, wrapped, reverse), func(t *testing.T) {
					pmd := NewPerMessageDeflateExtension()
					custom := &entryExtension{name: "entry-custom", decline: tc.decline}
					exts := []Extension{pmd, custom}
					if reverse {
						exts[0], exts[1] = exts[1], exts[0]
					}
					if tc.duplicate {
						exts = append(exts, NewPerMessageDeflateExtension())
					}
					r := upgradeRequest()
					r.Header.Set("Sec-WebSocket-Extensions", tc.offer)
					raw := newEntryConn(nil)
					w := &entryHijacker{ResponseRecorder: httptest.NewRecorder(), raw: raw}
					var response http.ResponseWriter = w
					if wrapped {
						response = &entryUnwrapper{response}
					}
					conn, err := Upgrade(response, r, WithUpgradeExtensions(exts...))
					if tc.reject {
						if conn != nil || !errors.Is(err, ErrUnsupportedExtensionComposition) {
							t.Fatalf("Upgrade = %v, %v; want composition rejection", conn, err)
						}
						if w.calls != 0 || w.Body.Len() != 0 || raw.reads != 0 || raw.writes != 0 || raw.written.Len() != 0 {
							t.Fatalf("rejected handshake committed I/O: hijacks=%d response=%q reads=%d writes=%d wire=%q", w.calls, w.Body.String(), raw.reads, raw.writes, raw.written.String())
						}
					} else {
						if err != nil || conn == nil || w.calls != 1 {
							t.Fatalf("Upgrade = %v, %v; hijacks=%d", conn, err, w.calls)
						}
						resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw.written.Bytes())), nil)
						if err != nil {
							t.Fatal(err)
						}
						if got := resp.Header.Get("Sec-WebSocket-Extensions"); got != tc.wantSelection {
							t.Fatalf("selection=%q, want %q", got, tc.wantSelection)
						}
						if resp.StatusCode != http.StatusSwitchingProtocols {
							t.Fatalf("status=%d", resp.StatusCode)
						}
					}
					if custom.negotiations != 1 || custom.incoming != 0 || custom.outgoing != 0 {
						t.Fatalf("custom callbacks: negotiations=%d incoming=%d outgoing=%d", custom.negotiations, custom.incoming, custom.outgoing)
					}
				})
			}
		}
	}
}

func TestPMDEntryDialComposition(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		selection                           []string
		decline, duplicate, reject, invalid bool
	}{
		{name: "PMD only selected", selection: []string{"permessage-deflate"}},
		{name: "custom only selected", selection: []string{"entry-custom"}},
		{name: "neither selected"},
		{name: "custom declines selection", selection: []string{"permessage-deflate, entry-custom"}, decline: true},
		{name: "PMD first mixed", selection: []string{"permessage-deflate, entry-custom"}, reject: true},
		{name: "custom first mixed", selection: []string{"entry-custom, permessage-deflate"}, reject: true},
		{name: "separate response fields", selection: []string{"entry-custom", "permessage-deflate"}, reject: true},
		{name: "multiple PMD owners", selection: []string{"permessage-deflate"}, duplicate: true, reject: true},
		{name: "multiple disabled PMD owners", selection: []string{"entry-custom"}, duplicate: true},
		{name: "unknown selection", selection: []string{"unknown"}, invalid: true},
		{name: "repeated PMD selection", selection: []string{"permessage-deflate, permessage-deflate"}, invalid: true},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reversed=%t", tc.name, reverse), func(t *testing.T) {
				pmd := NewPerMessageDeflateExtension()
				custom := &entryExtension{name: "entry-custom", decline: tc.decline}
				exts := []Extension{pmd, custom}
				if reverse {
					exts[0], exts[1] = exts[1], exts[0]
				}
				if tc.duplicate {
					exts = append(exts, NewPerMessageDeflateExtension())
				}
				var offers []string
				for _, e := range exts {
					offers = append(offers, e.Offer())
				}
				peerClosed := make(chan error, 1)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if got := r.Header.Get("Sec-WebSocket-Extensions"); got != strings.Join(offers, ", ") {
						t.Errorf("request offers=%q, want %q", got, strings.Join(offers, ", "))
					}
					raw, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						peerClosed <- err
						return
					}
					defer raw.Close()
					_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
					fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
					for _, value := range tc.selection {
						fmt.Fprintf(rw, "Sec-WebSocket-Extensions: %s\r\n", value)
					}
					rw.WriteString("\r\n")
					// Coalesce a frame with the headers. Dial may prefetch transport
					// bytes, but must not process this frame on composition failure.
					rw.Write([]byte{0x82, 1, 'x'})
					if err := rw.Flush(); err != nil {
						peerClosed <- err
						return
					}
					var b [1]byte
					n, err := rw.Read(b[:])
					if n != 0 {
						err = fmt.Errorf("unexpected WebSocket output %x", b[:n])
					}
					peerClosed <- err
				}))
				defer srv.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				conn, resp, err := Dial(ctx, "ws"+srv.URL[4:], WithExtensions(exts...))
				if conn != nil {
					defer conn.conn.Close()
				}
				if resp == nil {
					t.Fatalf("missing parsed handshake response: %v", err)
				}
				if tc.reject || tc.invalid {
					want := ErrUnsupportedExtensionComposition
					if tc.invalid {
						want = ErrInvalidExtension
					}
					if conn != nil || !errors.Is(err, want) {
						t.Fatalf("Dial = %v, %v; want %v", conn, err, want)
					}
					if custom.incoming != 0 || custom.outgoing != 0 {
						t.Fatalf("rejection called frame callbacks: incoming=%d outgoing=%d", custom.incoming, custom.outgoing)
					}
				} else {
					if conn == nil || err != nil {
						t.Fatalf("Dial=%v, %v", conn, err)
					}
					opcode, data, err := conn.ReadMessage()
					if err != nil || opcode != BinaryMessage || string(data) != "x" {
						t.Fatalf("first message=%v, %q, %v", opcode, data, err)
					}
					wantCalls := 0
					if custom.enabled {
						wantCalls = 1
					}
					if custom.incoming != wantCalls {
						t.Fatalf("incoming callbacks=%d, want %d", custom.incoming, wantCalls)
					}
					_ = conn.conn.Close() // Observe Dial failure closure without sending a normal Close.
				}
				select {
				case err := <-peerClosed:
					if !errors.Is(err, io.EOF) {
						t.Fatalf("peer did not observe transport release without frames: %v", err)
					}
				case <-time.After(4 * time.Second):
					t.Fatal("handshake transport remained open")
				}
			})
		}
	}
}

func TestPMDEntryManualCompositionBeforeIO(t *testing.T) {
	operations := []struct {
		name string
		run  func(*Conn) error
	}{
		{"read", func(c *Conn) error {
			_, data, err := c.ReadMessage()
			if len(data) != 0 {
				t.Error("rejected read returned data")
			}
			return err
		}},
		{"write", func(c *Conn) error { return c.WriteMessage(BinaryMessage, []byte("application")) }},
		{"ping", func(c *Conn) error { return c.WriteControlFrame(PingMessage, []byte("ping")) }},
		{"pong", func(c *Conn) error { return c.WriteControlFrame(PongMessage, []byte("pong")) }},
		{"close control", func(c *Conn) error { return c.WriteControlFrame(CloseMessage, nil) }},
		{"direct Close", func(c *Conn) error { return c.Close() }},
	}
	for _, afterConstruction := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			for _, op := range operations {
				t.Run(fmt.Sprintf("%s/postconstruction=%t/duplicate=%t", op.name, afterConstruction, duplicate), func(t *testing.T) {
					pmd := NewPerMessageDeflateExtension()
					custom := &entryExtension{name: "entry-custom"}
					exts := []Extension{pmd, custom}
					if duplicate {
						exts = []Extension{pmd, NewPerMessageDeflateExtension(), custom}
						custom.decline = true
					}
					enable := func() {
						for _, ext := range exts {
							if err := ext.Negotiate("permessage-deflate, entry-custom"); err != nil {
								t.Fatal(err)
							}
						}
					}
					if !afterConstruction {
						enable()
					}
					raw := newEntryConn([]byte{0x82, 1, 'x'})
					conn := NewConn(raw, false, exts)
					if afterConstruction {
						enable()
					}
					if err := op.run(conn); !errors.Is(err, ErrUnsupportedExtensionComposition) {
						t.Fatalf("first operation error=%v, want composition error", err)
					}
					if raw.reads != 0 || raw.writes != 0 || raw.written.Len() != 0 || custom.incoming != 0 || custom.outgoing != 0 {
						t.Fatalf("unsafe mixed-chain processing: reads=%d writes=%d wire=%x incoming=%d outgoing=%d", raw.reads, raw.writes, raw.written.Bytes(), custom.incoming, custom.outgoing)
					}
					if raw.closes != 1 {
						t.Fatalf("transport close count=%d, want 1", raw.closes)
					}
					// Resolving configuration after terminal failure must not resurrect
					// this transport or emit a raw Close that bypasses the custom chain.
					for _, ext := range exts {
						if err := ext.Negotiate(""); err != nil {
							t.Fatal(err)
						}
					}
					if err := conn.WriteMessage(BinaryMessage, []byte("later")); err == nil {
						t.Fatal("write resurrected failed connection")
					}
					if _, data, err := conn.ReadMessage(); err == nil || len(data) != 0 {
						t.Fatalf("read after rejection=%q, %v", data, err)
					}
					if err := conn.Close(); !errors.Is(err, ErrAlreadyClosed) {
						t.Fatalf("repeated Close=%v", err)
					}
					if raw.reads != 0 || raw.writes != 0 || raw.closes != 1 || custom.incoming != 0 || custom.outgoing != 0 {
						t.Fatalf("terminal operation caused I/O/callbacks: reads=%d writes=%d closes=%d incoming=%d outgoing=%d", raw.reads, raw.writes, raw.closes, custom.incoming, custom.outgoing)
					}
				})
			}
		}
	}
}

func TestPMDEntryManualPermittedCompositions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pmd, custom bool
	}{
		{"disabled custom", true, false}, {"custom only", false, true}, {"neither", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pmd := NewPerMessageDeflateExtension()
			custom := &entryExtension{name: "entry-custom", enabled: tc.custom}
			wire, want := []byte{0x82, 1, 'x'}, "x"
			if tc.pmd {
				wire = append([]byte{0xc2, byte(len(entryHello))}, entryHello...)
				want = "Hello"
			}
			raw := newEntryConn(wire)
			conn := NewConn(raw, false, []Extension{pmd, custom})
			if tc.pmd {
				if err := pmd.Negotiate("permessage-deflate"); err != nil {
					t.Fatal(err)
				}
			}
			if opcode, data, err := conn.ReadMessage(); err != nil || opcode != BinaryMessage || string(data) != want {
				t.Fatalf("read=%v, %q, %v", opcode, data, err)
			}
			if err := conn.WriteMessage(BinaryMessage, []byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := conn.WriteControlFrame(PingMessage, nil); err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			wantIn, wantOut := 0, 0
			if tc.custom {
				wantIn, wantOut = 1, 3
			}
			if custom.incoming != wantIn || custom.outgoing != wantOut {
				t.Fatalf("custom-only/disabled callbacks=%d,%d; want %d,%d", custom.incoming, custom.outgoing, wantIn, wantOut)
			}
		})
	}
}

func TestPMDEntryCompositionUsesBuiltinIdentity(t *testing.T) {
	for _, withBuiltin := range []bool{false, true} {
		t.Run(fmt.Sprintf("builtin=%t", withBuiltin), func(t *testing.T) {
			custom := &entryExtension{name: "permessage-deflate", enabled: true}
			exts := []Extension{custom}
			if withBuiltin {
				pmd := NewPerMessageDeflateExtension()
				if err := pmd.Negotiate("permessage-deflate"); err != nil {
					t.Fatal(err)
				}
				exts = append(exts, pmd)
			}
			raw := newEntryConn(nil)
			err := NewConn(raw, true, exts).WriteMessage(BinaryMessage, []byte("x"))
			if withBuiltin {
				if !errors.Is(err, ErrUnsupportedExtensionComposition) || custom.outgoing != 0 || raw.writes != 0 {
					t.Fatalf("named custom mixed with builtin: error=%v callbacks=%d writes=%d", err, custom.outgoing, raw.writes)
				}
			} else if err != nil || custom.outgoing != 1 || raw.writes == 0 {
				t.Fatalf("custom-only behavior changed: error=%v callbacks=%d writes=%d", err, custom.outgoing, raw.writes)
			}
		})
	}
}

// RFC 7692 section 7.2.3's second representation requires the first message's
// dictionary. These fixed wire vectors do not use this package's encoder.
var entryHello = []byte{0xf2, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00}
var entryHelloHistory = []byte{0xf2, 0x00, 0x11, 0x00, 0x00}

func TestPMDEntryDirectCallbacksMatrix(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			for _, final := range []bool{false, true} {
				for _, opcode := range []Opcode{TextMessage, BinaryMessage, ContinuationFrame, PingMessage, PongMessage, CloseMessage} {
					for _, rsv1 := range []bool{false, true} {
						t.Run(fmt.Sprintf("incoming=%t/enabled=%t/final=%t/opcode=%s/rsv1=%t", incoming, enabled, final, opcode, rsv1), func(t *testing.T) {
							pmd := NewPerMessageDeflateExtension()
							if enabled {
								if err := pmd.Negotiate("permessage-deflate"); err != nil {
									t.Fatal(err)
								}
							}
							payload := []byte("Hello")
							if incoming && rsv1 {
								payload = bytes.Clone(entryHello)
							}
							frame := Frame{Final: final, Opcode: opcode, Payload: payload, Rsv1: rsv1, Masked: true, MaskKey: [4]byte{1, 2, 3, 4}, Rsv2: true}
							before := frame
							before.Payload = bytes.Clone(frame.Payload)
							var err error
							if incoming {
								err = pmd.ProcessIncomingFrame(&frame)
							} else {
								err = pmd.ProcessOutgoingFrame(&frame)
							}
							data := opcode == TextMessage || opcode == BinaryMessage
							rejected := enabled && (opcode == ContinuationFrame || data && !final)
							if rejected {
								if !errors.Is(err, ErrFragmentedCompression) {
									t.Fatalf("error=%v, want ErrFragmentedCompression", err)
								}
								if !reflect.DeepEqual(frame, before) {
									t.Fatalf("rejected fragment changed: %#v -> %#v", before, frame)
								}
								return
							}
							if err != nil {
								t.Fatal(err)
							}
							if !enabled || !data || incoming && !rsv1 {
								if !reflect.DeepEqual(frame, before) {
									t.Fatalf("no-op changed frame: %#v -> %#v", before, frame)
								}
								return
							}
							if incoming {
								if string(frame.Payload) != "Hello" || frame.Rsv1 {
									t.Fatalf("decoded=%q RSV1=%t", frame.Payload, frame.Rsv1)
								}
							} else {
								if !frame.Rsv1 {
									t.Fatal("compressed outgoing frame missing RSV1")
								}
								r := flate.NewReader(bytes.NewReader(append(bytes.Clone(frame.Payload), 0, 0, 0xff, 0xff, 1, 0, 0, 0xff, 0xff)))
								got, err := io.ReadAll(r)
								_ = r.Close()
								if err != nil || string(got) != "Hello" {
									t.Fatalf("outgoing DEFLATE=%q, %v", got, err)
								}
							}
							if frame.Final != before.Final || frame.Opcode != before.Opcode || frame.Masked != before.Masked || frame.MaskKey != before.MaskKey || frame.Rsv2 != before.Rsv2 || frame.Rsv3 != before.Rsv3 {
								t.Fatal("transformation changed unrelated frame metadata")
							}
						})
					}
				}
			}
		}
	}
}

func TestPMDEntryDirectIncomingHistory(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, noContext := range []bool{false, true} {
			for _, interleave := range []bool{false, true} {
				t.Run(fmt.Sprintf("server=%t/no-context=%t/interleave=%t", server, noContext, interleave), func(t *testing.T) {
					pmd := NewPerMessageDeflateExtension()
					_ = NewConn(newEntryConn(nil), server, []Extension{pmd})
					selection := "permessage-deflate"
					if noContext {
						if server {
							selection += "; client_no_context_takeover"
						} else {
							selection += "; server_no_context_takeover"
						}
					}
					if err := pmd.Negotiate(selection); err != nil {
						t.Fatal(err)
					}
					for i := 0; i < 2; i++ {
						payload := entryHello
						if i == 1 && !noContext {
							payload = entryHelloHistory
						}
						frame := &Frame{Final: true, Opcode: TextMessage, Rsv1: true, Payload: bytes.Clone(payload)}
						if err := pmd.ProcessIncomingFrame(frame); err != nil || string(frame.Payload) != "Hello" || frame.Rsv1 {
							t.Fatalf("message %d=%q, %v, RSV1=%t", i, frame.Payload, err, frame.Rsv1)
						}
						if interleave && i == 0 {
							for _, op := range []Opcode{TextMessage, BinaryMessage, PingMessage, PongMessage, CloseMessage} {
								frame := &Frame{Final: true, Opcode: op, Payload: []byte("uncompressed interleave")}
								if err := pmd.ProcessIncomingFrame(frame); err != nil || string(frame.Payload) != "uncompressed interleave" {
									t.Fatalf("interleaved %s=%q, %v", op, frame.Payload, err)
								}
							}
						}
					}
					if noContext {
						frame := &Frame{Final: true, Opcode: BinaryMessage, Rsv1: true, Payload: bytes.Clone(entryHelloHistory)}
						if err := pmd.ProcessIncomingFrame(frame); err == nil {
							t.Fatal("no-context receiver reused previous message history")
						}
					}
				})
			}
		}
	}
}

func TestPMDEntryDirectBFINALAndRejectedFragment(t *testing.T) {
	for _, noContext := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-context=%t", noContext), func(t *testing.T) {
			pmd := NewPerMessageDeflateExtension()
			selection := "permessage-deflate"
			if noContext {
				selection += "; server_no_context_takeover"
			}
			if err := pmd.Negotiate(selection); err != nil {
				t.Fatal(err)
			}
			// A rejected complete DEFLATE payload in a nonfinal callback must
			// not be retained and concatenated into the next complete message.
			fragment := &Frame{Opcode: TextMessage, Rsv1: true, Payload: bytes.Clone(entryHello)}
			if err := pmd.ProcessIncomingFrame(fragment); !errors.Is(err, ErrFragmentedCompression) {
				t.Fatalf("fragment error=%v", err)
			}
			// Two finalized streams. The second requires same-message history,
			// including when cross-message context takeover is disabled.
			frame := &Frame{Final: true, Opcode: TextMessage, Rsv1: true, Payload: []byte{0xf3, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x00, 0xf3, 0x00, 0x11, 0x00, 0x00}}
			if err := pmd.ProcessIncomingFrame(frame); err != nil || string(frame.Payload) != "HelloHello" || frame.Rsv1 {
				t.Fatalf("BFINAL pair=%q, %v, RSV1=%t", frame.Payload, err, frame.Rsv1)
			}
			frame = &Frame{Final: true, Opcode: BinaryMessage, Rsv1: true, Payload: bytes.Clone(entryHello)}
			if err := pmd.ProcessIncomingFrame(frame); err != nil || string(frame.Payload) != "Hello" {
				t.Fatalf("next message=%q, %v", frame.Payload, err)
			}
		})
	}
}

// Manual negotiation is allowed after NewConn and may change between operations.
// An earlier valid operation must not permanently cache an allowed composition.
func TestPMDEntryManualCompositionRevalidated(t *testing.T) {
	for _, operation := range []string{"read", "write", "control", "Close"} {
		t.Run(operation, func(t *testing.T) {
			pmd := NewPerMessageDeflateExtension()
			custom := &entryExtension{name: "entry-custom"}
			raw := newEntryConn([]byte{0x82, 1, 'x'})
			conn := NewConn(raw, false, []Extension{pmd, custom})
			if err := conn.WriteControlFrame(PongMessage, nil); err != nil {
				t.Fatal(err)
			}
			beforeWrites, beforeWire := raw.writes, bytes.Clone(raw.written.Bytes())
			for _, ext := range []Extension{pmd, custom} {
				if err := ext.Negotiate("permessage-deflate, entry-custom"); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch operation {
			case "read":
				_, _, err = conn.ReadMessage()
			case "write":
				err = conn.WriteMessage(BinaryMessage, []byte("later"))
			case "control":
				err = conn.WriteControlFrame(PingMessage, nil)
			case "Close":
				err = conn.Close()
			}
			if !errors.Is(err, ErrUnsupportedExtensionComposition) {
				t.Fatalf("later operation=%v", err)
			}
			if raw.reads != 0 || raw.writes != beforeWrites || !bytes.Equal(raw.written.Bytes(), beforeWire) || raw.closes != 1 || custom.incoming != 0 || custom.outgoing != 0 {
				t.Fatalf("later invalid composition performed I/O: reads=%d writes=%d closes=%d incoming=%d outgoing=%d", raw.reads, raw.writes, raw.closes, custom.incoming, custom.outgoing)
			}
		})
	}
}

func TestPMDEntryRejectedDirectFragmentDoesNotSeedHistory(t *testing.T) {
	for _, opcode := range []Opcode{TextMessage, BinaryMessage, ContinuationFrame} {
		t.Run(opcode.String(), func(t *testing.T) {
			pmd := NewPerMessageDeflateExtension()
			if err := pmd.Negotiate("permessage-deflate"); err != nil {
				t.Fatal(err)
			}
			fragment := &Frame{Opcode: opcode, Rsv1: true, Payload: bytes.Clone(entryHello)}
			if err := pmd.ProcessIncomingFrame(fragment); !errors.Is(err, ErrFragmentedCompression) {
				t.Fatalf("fragment=%v", err)
			}
			dependent := &Frame{Final: true, Opcode: TextMessage, Rsv1: true, Payload: bytes.Clone(entryHelloHistory)}
			if err := pmd.ProcessIncomingFrame(dependent); err == nil {
				t.Fatal("rejected fragment committed a dictionary")
			}
		})
	}
}
