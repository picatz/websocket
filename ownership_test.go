package websocket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

func ownershipPayload(op Opcode, size int) []byte {
	if op == TextMessage {
		return bytes.Repeat([]byte{'a'}, size)
	}
	return benchmarkPayload(size)
}

// Capacities are checked against this toolchain's append behavior rather than
// hard-coding io.ReadAll's growth policy. Capacity is not physical heap usage;
// allocation benchmarks separately establish that eligible frames avoid a copy.
func TestReadMessageOwnershipCapacityAndLifetime(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, op := range []Opcode{TextMessage, BinaryMessage} {
			for _, size := range []int{0, 1, 16, 511, 512, 513, 4096, 4097, 65536, 65537} {
				t.Run(fmt.Sprintf("server=%v/type=%s/bytes=%d", server, op, size), func(t *testing.T) {
					want := ownershipPayload(op, size)
					frameWire := limitWire(server, 0x80|byte(op), want)
					frame, err := frameConn(frameWire, server, 0).readFrame()
					if err != nil {
						t.Fatal(err)
					}
					wantCap := cap(append([]byte(nil), want...))
					if size > 0 && cap(frame.Payload) == size {
						wantCap = size
					}
					wire := append(bytes.Clone(frameWire), frameWire...)
					// An allocated but empty extension slice also registers no extensions.
					c := NewConn(&memoryConn{Reader: bytes.NewReader(wire)}, server, []Extension{})
					kind, first, err := c.ReadMessage()
					if err != nil || kind != op || !bytes.Equal(first, want) || cap(first) != wantCap || (size == 0 && first != nil) {
						t.Fatalf("first read: type=%v len/cap=%d/%d err=%v; want %v %d/%d", kind, len(first), cap(first), err, op, size, wantCap)
					}
					if size > 0 {
						first[0] ^= 0xff
					}
					retained := bytes.Clone(first)
					kind, second, err := c.ReadMessage()
					if err != nil || kind != op || !bytes.Equal(second, want) || !bytes.Equal(first, retained) {
						t.Fatalf("later read changed a message: type=%v err=%v", kind, err)
					}
					if !bytes.Equal(wire[:len(frameWire)], frameWire) || !bytes.Equal(wire[len(frameWire):], frameWire) {
						t.Fatal("returned payload aliases input wire")
					}
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}
					clear(wire)
					if !bytes.Equal(first, retained) || !bytes.Equal(second, want) {
						t.Fatal("retained payload changed after Close or wire mutation")
					}
					if size > 0 {
						second[0] ^= 0x33
						if !bytes.Equal(first, retained) {
							t.Fatal("returned messages share storage after Close")
						}
					}
				})
			}
		}
	}
}

func TestReadMessageOwnershipFragmentAndControlBoundaries(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, op := range []Opcode{TextMessage, BinaryMessage} {
			payload := ownershipPayload(op, 512)
			frame := func(first byte, p []byte) []byte { return limitWire(server, first, p) }
			for _, tc := range []struct {
				name string
				wire []byte
				want []byte
			}{
				{"controls before final", bytes.Join([][]byte{frame(0x89, []byte("ping")), frame(0x8a, nil), frame(0x80|byte(op), payload)}, nil), payload},
				{"nonfinal first", bytes.Join([][]byte{frame(byte(op), payload), frame(0x80, nil)}, nil), payload},
				{"empty first", bytes.Join([][]byte{frame(byte(op), nil), frame(0x80, payload)}, nil), payload},
				{"empty chain with controls", bytes.Join([][]byte{frame(byte(op), nil), frame(0x89, []byte("ping")), frame(0x00, nil), frame(0x8a, nil), frame(0x00, nil), frame(0x80, payload)}, nil), payload},
				{"all empty", bytes.Join([][]byte{frame(byte(op), nil), frame(0x00, nil), frame(0x80, nil)}, nil), nil},
			} {
				t.Run(fmt.Sprintf("server=%v/type=%s/%s", server, op, tc.name), func(t *testing.T) {
					c := frameConn(tc.wire, server, len(payload))
					kind, got, err := c.ReadMessage()
					if err != nil || kind != op || !bytes.Equal(got, tc.want) || (tc.want == nil && got != nil) {
						t.Fatalf("read: type=%v len=%d err=%v", kind, len(got), err)
					}
				})
			}
			// An empty first fragment still starts a message. Reject a second
			// starter from its header, before reading its eligible-sized body.
			t.Run(fmt.Sprintf("server=%v/type=%s/second starter", server, op), func(t *testing.T) {
				wire := append(frame(byte(op), nil), limitHeader(server, 0x80|byte(op), len(payload))...)
				raw := &headerOnlyConn{memoryConn: memoryConn{Reader: bytes.NewReader(wire)}}
				c := NewConn(raw, server, nil)
				if _, p, err := c.ReadMessage(); err != ErrUnexpectedFrame || p != nil || raw.bodyReads != 0 {
					t.Fatalf("read: len=%d err=%v body reads=%d", len(p), err, raw.bodyReads)
				}
				ownershipRequireTerminal(t, c, &raw.memoryConn, server, 1002)
			})
		}
		// Keep one integrated cross-frame UTF-8 case; exhaustive network
		// chunk, mask-phase and early rejection cases live in streaming_utf8_test.
		wire := bytes.Join([][]byte{limitWire(server, 0x01, []byte{'a', 0xe2}), limitWire(server, 0x8a, nil), limitWire(server, 0x00, nil), limitWire(server, 0x80, []byte{0x82, 0xac})}, nil)
		if op, p, err := frameConn(wire, server, 0).ReadMessage(); err != nil || op != TextMessage || string(p) != "a€" {
			t.Fatalf("server=%v: split UTF-8: type=%v payload=%x err=%v", server, op, p, err)
		}
	}
}

type ownershipExtension struct {
	Extension
	enabled                    bool
	enabledCalls, processCalls int
	process                    func(*Frame) error
}

func (*ownershipExtension) Name() string { return "ownership-test" }
func (e *ownershipExtension) IsEnabled() bool {
	e.enabledCalls++
	return e.enabled
}
func (e *ownershipExtension) ProcessIncomingFrame(f *Frame) error {
	e.processCalls++
	if e.process != nil {
		return e.process(f)
	}
	return nil
}

func TestReadMessageOwnershipExtensionIsolation(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, op := range []Opcode{TextMessage, BinaryMessage} {
			for _, replace := range []bool{false, true} {
				t.Run(fmt.Sprintf("server=%v/type=%s/shared scratch=%v", server, op, replace), func(t *testing.T) {
					firstWant, secondWant := bytes.Repeat([]byte{'a'}, 512), bytes.Repeat([]byte{'b'}, 512)
					scratch := make([]byte, 512)
					var held []*Frame
					ext := &ownershipExtension{enabled: true, process: func(f *Frame) error {
						if replace {
							copy(scratch, f.Payload)
							f.Payload = scratch
						}
						held = append(held, f)
						return nil
					}}
					wire := append(limitWire(server, 0x80|byte(op), firstWant), limitWire(server, 0x80|byte(op), secondWant)...)
					c := NewConn(&memoryConn{Reader: bytes.NewReader(wire)}, server, []Extension{ext})
					kind, first, err := c.ReadMessage()
					if err != nil || kind != op || !bytes.Equal(first, firstWant) || len(held) != 1 {
						t.Fatalf("first read: type=%v len=%d err=%v held=%d", kind, len(first), err, len(held))
					}
					held[0].Payload[0] = 'h'
					if !bytes.Equal(first, firstWant) {
						t.Fatal("retained extension frame aliases returned payload")
					}
					first[1] = 'z'
					if held[0].Payload[1] != 'a' {
						t.Fatal("caller mutation changes extension storage")
					}
					retained := bytes.Clone(first)
					kind, second, err := c.ReadMessage()
					if err != nil || kind != op || !bytes.Equal(second, secondWant) || !bytes.Equal(first, retained) || len(held) != 2 {
						t.Fatalf("second read: type=%v len=%d err=%v held=%d", kind, len(second), err, len(held))
					}
					held[1].Payload[0] = 'j'
					second[1] = 'q'
					if second[0] != 'b' || held[1].Payload[1] != 'b' || !bytes.Equal(first, retained) {
						t.Fatal("later extension or caller mutation changed another owner")
					}
				})
			}
		}
	}
}

func TestReadMessageOwnershipRegisteredExtensions(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, op := range []Opcode{TextMessage, BinaryMessage} {
			for _, enabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("server=%v/type=%s/enabled=%v", server, op, enabled), func(t *testing.T) {
					want := ownershipPayload(op, 512)
					wire := limitWire(server, 0x80|byte(op), want)
					ext := &ownershipExtension{enabled: enabled}
					c := NewConn(&memoryConn{Reader: bytes.NewReader(wire)}, server, []Extension{ext})
					kind, got, err := c.ReadMessage()
					wantProcess := 0
					if enabled {
						wantProcess = 1
					}
					// One IsEnabled call for header validation and one for payload
					// processing. The ownership gate must not query it again.
					if err != nil || kind != op || !bytes.Equal(got, want) || ext.enabledCalls != 2 || ext.processCalls != wantProcess {
						t.Fatalf("read: type=%v err=%v callbacks=%d/%d, want 2/%d", kind, err, ext.enabledCalls, ext.processCalls, wantProcess)
					}
					// RSV1 is deliberately clear: even enabled PMD must keep the
					// registered-extension fallback for uncompressed messages.
					pmd := &perMessageDeflate{enabled: enabled}
					c = NewConn(&memoryConn{Reader: bytes.NewReader(wire)}, server, []Extension{pmd})
					if kind, got, err = c.ReadMessage(); err != nil || kind != op || !bytes.Equal(got, want) {
						t.Fatalf("uncompressed PMD: type=%v len=%d err=%v", kind, len(got), err)
					}
				})
			}
		}
	}
}

func TestReadMessageOwnershipDecodedLimits(t *testing.T) {
	const limit = 512
	for _, server := range []bool{false, true} {
		for _, op := range []Opcode{TextMessage, BinaryMessage} {
			for _, size := range []int{0, limit, limit + 1} {
				t.Run(fmt.Sprintf("server=%v/type=%s/decoded=%d", server, op, size), func(t *testing.T) {
					shared := bytes.Repeat([]byte{'a'}, limit+1)
					grow := &ownershipExtension{enabled: true, process: func(f *Frame) error {
						f.Payload = shared
						return nil
					}}
					trim := &ownershipExtension{enabled: true, process: func(f *Frame) error {
						// Full slicing makes the retained extension output eligible
						// by capacity alone; it still must not be adopted.
						f.Payload = f.Payload[:size:size]
						return nil
					}}
					raw := &failureMemoryConn{memoryConn: memoryConn{Reader: bytes.NewReader(limitWire(server, 0x80|byte(op), ownershipPayload(op, limit)))}}
					c := NewConn(raw, server, []Extension{grow, trim}, WithMaxBytes(limit))
					kind, got, err := c.ReadMessage()
					if grow.processCalls != 1 || trim.processCalls != 1 {
						t.Fatalf("decoded limit checked before all extensions: grow=%d trim=%d", grow.processCalls, trim.processCalls)
					}
					if size > limit {
						if err != ErrPayloadTooLarge || got != nil {
							t.Fatalf("overflow: len=%d err=%v", len(got), err)
						}
						// Enabled custom extensions suppress automatic Close encoding.
						ownershipRequireTerminal(t, c, &raw.memoryConn, server, 0)
						if raw.closes != 1 {
							t.Fatalf("transport closes=%d, want 1", raw.closes)
						}
						return
					}
					if err != nil || kind != op || !bytes.Equal(got, shared[:size]) || (size == 0 && got != nil) {
						t.Fatalf("trimmed output: type=%v len=%d err=%v", kind, len(got), err)
					}
					shared[0] = 'z'
					if size > 0 && got[0] != 'a' {
						t.Fatal("trimmed extension output aliases returned payload")
					}
				})
			}
		}
	}
}

func ownershipRequireTerminal(t *testing.T, c *Conn, raw *memoryConn, server bool, closeCode uint16) {
	t.Helper()
	if closeCode == 0 {
		if raw.written.Len() != 0 {
			t.Fatalf("unexpected close output: %x", raw.written.Bytes())
		}
	} else {
		p := failureClosePayload(t, raw.written.Bytes(), server)
		if len(p) != 2 || binary.BigEndian.Uint16(p) != closeCode {
			t.Fatalf("close payload=%x, want code %d", p, closeCode)
		}
	}
	before := bytes.Clone(raw.written.Bytes())
	if _, p, err := c.ReadMessage(); err != io.ErrClosedPipe || p != nil {
		t.Fatalf("terminal read: len=%d err=%v", len(p), err)
	}
	if err := c.WriteMessage(BinaryMessage, nil); err != io.ErrClosedPipe {
		t.Fatalf("terminal write: %v", err)
	}
	if err := c.Close(); err != ErrAlreadyClosed {
		t.Fatalf("terminal Close: %v", err)
	}
	if !bytes.Equal(raw.written.Bytes(), before) {
		t.Fatal("terminal operation wrote another frame")
	}
}

func TestReadMessageOwnershipFailures(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, op := range []Opcode{TextMessage, BinaryMessage} {
			for _, mode := range []string{"truncated", "transport error", "wire limit", "extension error", "invalid UTF-8", "unfinished UTF-8"} {
				if op != TextMessage && (mode == "invalid UTF-8" || mode == "unfinished UTF-8") {
					continue
				}
				t.Run(fmt.Sprintf("server=%v/type=%s/%s", server, op, mode), func(t *testing.T) {
					p := ownershipPayload(op, 4096)
					cause, tail := error(io.ErrUnexpectedEOF), error(io.EOF)
					var closeCode uint16
					var extensions []Extension
					limit := 0
					switch mode {
					case "transport error":
						cause, tail = streamWithheld, streamWithheld
					case "wire limit":
						cause, closeCode, limit = ErrPayloadTooLarge, 1009, len(p)-1
					case "extension error":
						cause = fmt.Errorf("custom failure: %w", io.EOF)
						extensions = []Extension{&ownershipExtension{enabled: true, process: func(*Frame) error { return cause }}}
					case "invalid UTF-8":
						p[len(p)-1] = 0xff
						cause, closeCode = ErrInvalidFrame, 1007
					case "unfinished UTF-8":
						// The complete 4096-byte frame reaches the ownership gate,
						// but its unfinished final rune still must fail afterwards.
						p[len(p)-1] = 0xc2
						cause, closeCode = ErrInvalidFrame, 1007
					}
					wire := limitWire(server, 0x80|byte(op), p)
					if mode == "truncated" || mode == "transport error" {
						wire = wire[:len(wire)-1]
					} else if mode == "wire limit" {
						wire = limitHeader(server, 0x80|byte(op), len(p))
					}
					raw := &streamChoppedConn{chunks: [][]byte{wire}, tailErr: tail}
					c := NewConn(raw, server, extensions, WithMaxBytes(limit))
					_, got, err := c.ReadMessage()
					if !errors.Is(err, cause) || got != nil {
						t.Fatalf("failure: len=%d err=%v, want %v", len(got), err, cause)
					}
					if closeCode != 0 && err != cause {
						t.Fatalf("sentinel identity changed: %v, want exact %v", err, cause)
					}
					if mode == "extension error" && errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatal("extension EOF was mistaken for transport truncation")
					}
					if mode == "wire limit" && raw.tailReads != 0 {
						t.Fatal("oversize header caused a body read")
					}
					ownershipRequireTerminal(t, c, &raw.memoryConn, server, closeCode)
					if raw.closes != 1 {
						t.Fatalf("transport closes=%d, want 1", raw.closes)
					}
				})
			}
		}
	}
}

// Hold the first transport Read open so lock ownership is tested at a known
// point, rather than relying on two goroutines happening to overlap.
type ownershipGatedConn struct {
	memoryConn
	once             sync.Once
	entered, release chan struct{}
}

func (c *ownershipGatedConn) Read(p []byte) (int, error) {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return c.memoryConn.Read(p)
}

func TestReadMessageOwnershipConcurrentReaders(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, op := range []Opcode{TextMessage, BinaryMessage} {
			t.Run(fmt.Sprintf("server=%v/type=%s", server, op), func(t *testing.T) {
				wire := append(limitWire(server, 0x80|byte(op), bytes.Repeat([]byte{'a'}, 4096)), limitWire(server, 0x80|byte(op), bytes.Repeat([]byte{'b'}, 4096))...)
				raw := &ownershipGatedConn{memoryConn: memoryConn{Reader: bytes.NewReader(wire)}, entered: make(chan struct{}), release: make(chan struct{})}
				c := NewConn(raw, server, nil)
				var release sync.Once
				unblock := func() { release.Do(func() { close(raw.release) }) }
				defer unblock()
				type result struct {
					op   Opcode
					data []byte
					err  error
				}
				results := make(chan result, 2)
				read := func() {
					kind, p, err := c.ReadMessage()
					if len(p) > 0 {
						p[len(p)-1] = p[0] - ('a' - 'A')
					}
					results <- result{kind, p, err}
				}
				go read()
				select {
				case <-raw.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("first reader did not enter transport")
				}
				if c.readMu.TryLock() {
					c.readMu.Unlock()
					t.Fatal("ReadMessage released its serialization lock during a frame read")
				}
				started := make(chan struct{})
				go func() { close(started); read() }()
				<-started
				unblock()
				seen := map[byte]bool{}
				var retained [][]byte
				for range 2 {
					select {
					case r := <-results:
						if r.err != nil || r.op != op || len(r.data) != 4096 {
							t.Fatalf("concurrent read: type=%v len=%d err=%v", r.op, len(r.data), r.err)
						}
						seen[r.data[0]] = true
						retained = append(retained, r.data)
					case <-time.After(5 * time.Second):
						t.Fatal("serialized readers did not finish")
					}
				}
				if !seen['a'] || !seen['b'] {
					t.Fatal("serialized readers lost or duplicated a message")
				}
				for _, p := range retained {
					if !bytes.Equal(p[:len(p)-1], bytes.Repeat(p[:1], len(p)-1)) || p[len(p)-1] != p[0]-('a'-'A') {
						t.Fatal("one concurrent caller changed another caller's payload")
					}
				}
			})
		}
	}
}
