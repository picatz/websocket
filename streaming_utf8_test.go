package websocket

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"
	"unicode/utf8"
)

var streamWithheld = errors.New("stream test: payload remainder is withheld")

type streamChoppedConn struct {
	memoryConn
	chunks    [][]byte
	tailErr   error
	tailReads int
	closes    int
}

func (c *streamChoppedConn) Read(p []byte) (int, error) {
	for len(c.chunks) > 0 && len(c.chunks[0]) == 0 {
		c.chunks = c.chunks[1:]
	}
	if len(c.chunks) == 0 {
		c.tailReads++
		return 0, c.tailErr
	}
	n := copy(p, c.chunks[0])
	c.chunks[0] = c.chunks[0][n:]
	return n, nil
}
func (c *streamChoppedConn) Close() error { c.closes++; return nil }

func streamParts(server bool, payload []byte, splits ...int) [][]byte {
	wire := failureWire(server, 0x01, payload)
	h := 2
	if server {
		h += 4
	}
	parts := [][]byte{wire[:h]}
	at := 0
	for _, end := range splits {
		parts = append(parts, wire[h+at:h+end])
		at = end
	}
	return parts
}

func TestReadMessageRejectsUnfinishedTextFrame(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, prefix := range [][]byte{{0x80}, {0xc0}, {0xc1}, {0xf5}, {0xf6}, {0xf7}, {0xf8}, {0xf9}, {0xfa}, {0xfb}, {0xfc}, {0xfd}, {0xfe}, {0xff}, {0xc2, 'x'}, {0xe0, 0x9f}, {0xed, 0xa0}, {0xf0, 0x8f}, {0xf4, 0x90}} {
			for split := 0; split <= len(prefix); split++ {
				t.Run(fmt.Sprintf("server=%v/prefix=%x/split=%d", server, prefix, split), func(t *testing.T) {
					payload := append(append([]byte("ok"), prefix...), []byte("withheld")...)
					c := &streamChoppedConn{chunks: streamParts(server, payload, 2+split, 2+len(prefix)), tailErr: streamWithheld}
					ws := NewConn(c, server, nil)
					_, p, err := ws.ReadMessage()
					if err != ErrInvalidFrame || p != nil || c.tailReads != 0 || c.closes != 1 {
						t.Fatalf("err=%v p=%x tailReads=%d closes=%d", err, p, c.tailReads, c.closes)
					}
					if code := binary.BigEndian.Uint16(failureClosePayload(t, c.written.Bytes(), server)); code != 1007 {
						t.Fatalf("close code %d", code)
					}
					before := bytes.Clone(c.written.Bytes())
					if _, _, err := ws.ReadMessage(); err != io.ErrClosedPipe {
						t.Fatal(err)
					}
					if err := ws.WriteMessage(TextMessage, []byte("later")); err != io.ErrClosedPipe {
						t.Fatal(err)
					}
					if c.closes != 1 || !bytes.Equal(before, c.written.Bytes()) {
						t.Fatal("terminal failure was repeated")
					}
				})
			}
		}
	}
}

func TestReadMessageIncompleteRuneEOFAndResume(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, prefix := range [][]byte{{0xc2}, {0xe0, 0xa0}, {0xf0, 0x90, 0x80}} {
			payload := append(append([]byte{}, prefix...), 0x80)
			raw := &streamChoppedConn{chunks: streamParts(server, payload, len(prefix)), tailErr: io.EOF}
			_, _, err := NewConn(raw, server, nil).ReadMessage()
			if !errors.Is(err, io.ErrUnexpectedEOF) || raw.written.Len() != 0 {
				t.Fatalf("prefix=%x err=%v written=%x", prefix, err, raw.written.Bytes())
			}
		}
		raw, peer := net.Pipe()
		c := NewConn(raw, server, nil)
		done := make(chan error, 1)
		go func() {
			op, p, err := c.ReadMessage()
			if err == nil && (op != TextMessage || !bytes.Equal(p, []byte("a𐀀z"))) {
				err = fmt.Errorf("bad result %v %x", op, p)
			}
			done <- err
		}()
		wire := failureWire(server, 0x81, []byte("a𐀀z"))
		h := 2
		if server {
			h += 4
		}
		peer.SetDeadline(time.Now().Add(time.Second))
		if _, err := peer.Write(wire[:h+3]); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("returned before rune was complete: %v", err)
		default:
		}
		if _, err := peer.Write(wire[h+3:]); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		raw.Close()
		peer.Close()
	}
}

func TestReadMessagePartialTextDeadlineTerminal(t *testing.T) {
	for _, server := range []bool{false, true} {
		raw, peer := net.Pipe()
		c := NewConn(raw, server, nil)
		if err := raw.SetReadDeadline(time.Now().Add(75 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, _, err := c.ReadMessage(); done <- err }()
		wire := failureWire(server, 0x81, []byte("𐀀"))
		h := 2
		if server {
			h += 4
		}
		if _, err := peer.Write(wire[:h+2]); err != nil {
			t.Fatal(err)
		}
		err := <-done
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline cause lost: %v", err)
		}
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("timeout type lost: %v", err)
		}
		if _, _, err := c.ReadMessage(); err != io.ErrClosedPipe {
			t.Fatalf("retry not terminal: %v", err)
		}
		if err := c.WriteMessage(TextMessage, []byte("later")); err != io.ErrClosedPipe {
			t.Fatalf("write not terminal: %v", err)
		}
		raw.Close()
		peer.Close()
	}
}

func TestReadFramePreservesRawTextContract(t *testing.T) {
	for _, server := range []bool{false, true} {
		f, err := frameConn(failureWire(server, 0x81, []byte{0xff}), server, 0).readFrame()
		if err != nil || f.Opcode != TextMessage || !bytes.Equal(f.Payload, []byte{0xff}) {
			t.Fatalf("readFrame policy changed: %v %v", f, err)
		}
	}
}

type streamChunkReader struct {
	data         []byte
	sizes        []int
	index        int
	tail         error
	endWithData  bool
	dataReadSize int
}

func (r *streamChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.tail
	}
	r.dataReadSize = len(p)
	n := len(r.data)
	if len(r.sizes) > 0 {
		n = min(n, r.sizes[r.index%len(r.sizes)])
		r.index++
	}
	n = copy(p, r.data[:n])
	r.data = r.data[n:]
	if len(r.data) == 0 && r.endWithData {
		return n, r.tail
	}
	return n, nil
}

func TestTextPayloadReaderUTF8EveryBoundaryAndMaskPhase(t *testing.T) {
	text := []byte("\x00\x7f\u0080\u07ff\u0800\ud7ff\ue000\ufffd\uffff\U00010000\U0010ffff")
	for _, masked := range []bool{false, true} {
		for split := 0; split <= len(text); split++ {
			for _, chunk := range []int{1, 2, 3, 4, 5, 7, 8, 9, 511, 512, 513} {
				wire := bytes.Clone(text)
				key := [4]byte{0x11, 0x22, 0x33, 0x44}
				if masked {
					for i := range wire {
						wire[i] ^= key[i&3]
					}
				}
				source := &streamChunkReader{data: wire, sizes: []int{max(1, split), chunk}, tail: io.EOF}
				rr := &textPayloadReader{r: bufio.NewReaderSize(source, 16), remaining: int64(len(wire)), key: key, masked: masked}
				got, err := io.ReadAll(rr)
				if err != nil || !bytes.Equal(got, text) || rr.pending != 0 {
					t.Fatalf("masked=%v split=%d chunk=%d got=%x pending=%d err=%v", masked, split, chunk, got, rr.pending, err)
				}
			}
		}
	}
}

func TestTextPayloadReaderCrossFrameCarryAndErrors(t *testing.T) {
	valid := []byte("a¢€𐀀�")
	for a := 0; a <= len(valid); a++ {
		for b := a; b <= len(valid); b++ {
			var tail []byte
			for _, p := range [][]byte{valid[:a], valid[a:b], nil, valid[b:]} {
				rr := &textPayloadReader{r: bufio.NewReader(&streamChunkReader{data: p, sizes: []int{1}, tail: io.EOF}), remaining: int64(len(p))}
				rr.pending = byte(copy(rr.carry[:], tail))
				got, err := io.ReadAll(rr)
				if err != nil || !bytes.Equal(got, p) {
					t.Fatalf("split=%d/%d got=%x err=%v", a, b, got, err)
				}
				tail = bytes.Clone(rr.carry[:rr.pending])
			}
			if len(tail) != 0 {
				t.Fatalf("split=%d/%d tail=%x", a, b, tail)
			}
		}
	}
	for _, tailErr := range []error{io.EOF, streamWithheld} {
		for _, p := range [][]byte{{'a'}, {0xf4}, {0xf4, 0x90}} {
			source := &streamChunkReader{data: p, sizes: []int{512}, tail: tailErr, endWithData: true}
			rr := &textPayloadReader{r: bufio.NewReaderSize(source, 16), remaining: 512}
			got, err := io.ReadAll(rr)
			if source.dataReadSize <= 16 {
				t.Fatalf("test did not exercise direct buffered read: %d", source.dataReadSize)
			}
			if bytes.Equal(p, []byte{0xf4, 0x90}) {
				failure, ok := err.(*readFailure)
				if !ok || failure.code != 1007 || failure.cause != ErrInvalidFrame {
					t.Fatalf("typed invalid err=%v", err)
				}
			} else if (tailErr == io.EOF && err != nil) || (tailErr != io.EOF && !errors.Is(err, tailErr)) {
				t.Fatalf("error precedence p=%x got=%x err=%v", p, got, err)
			}
		}
	}
}

func TestTextPayloadReaderRejectsBeforeWithheldSuffix(t *testing.T) {
	for _, masked := range []bool{false, true} {
		for _, prefix := range [][]byte{{0x80}, {0xc0}, {0xc1}, {0xf5}, {0xf6}, {0xf7}, {0xf8}, {0xf9}, {0xfa}, {0xfb}, {0xfc}, {0xfd}, {0xfe}, {0xff}, {0xc2, 'x'}, {0xe0, 0x9f}, {0xed, 0xa0}, {0xf0, 0x8f}, {0xf4, 0x90}} {
			for split := 0; split <= len(prefix); split++ {
				plain := append([]byte("ok"), prefix...)
				wire := bytes.Clone(plain)
				key := [4]byte{0x11, 0x22, 0x33, 0x44}
				if masked {
					for i := range wire {
						wire[i] ^= key[i&3]
					}
				}
				raw := &streamChoppedConn{chunks: [][]byte{wire[:2+split], wire[2+split:]}, tailErr: streamWithheld}
				rr := &textPayloadReader{r: bufio.NewReader(raw), remaining: int64(len(wire) + 100), key: key, masked: masked}
				_, err := io.ReadAll(rr)
				failure, ok := err.(*readFailure)
				if !ok || failure.code != 1007 || failure.cause != ErrInvalidFrame || raw.tailReads != 0 {
					t.Fatalf("masked=%v prefix=%x split=%d err=%v tailreads=%d", masked, prefix, split, err, raw.tailReads)
				}
			}
		}
	}
}

func TestReadMessagePreservesExtensionOwnership(t *testing.T) {
	shared := []byte("alpha")
	ext := &headerTransformExtension{enabled: true, process: func(f *Frame) error { f.Payload = shared; return nil }}
	raw := &memoryConn{Reader: bytes.NewReader([]byte{0x81, 1, 'x', 0x81, 1, 'y'})}
	c := NewConn(raw, false, []Extension{ext})
	_, got, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	shared[0] = 'X'
	if string(got) != "alpha" {
		t.Fatalf("returned data aliases extension: %q", got)
	}
	got[1] = 'Z'
	if shared[1] != 'l' {
		t.Fatal("caller data aliases extension")
	}
	first := bytes.Clone(got)
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, got) {
		t.Fatal("later message changed old result")
	}
}

func TestTextPayloadReaderLargeChunkMaskOffsets(t *testing.T) {
	payload := bytes.Repeat([]byte("a¢€𐀀"), 513)
	key := [4]byte{0x9d, 0x62, 0xab, 0xc4}
	for _, size := range []int{1, 2, 3, 4, 5, 7, 8, 9, 511, 512, 513, 4095, 4096, 4097} {
		wire := bytes.Clone(payload)
		for i := range wire {
			wire[i] ^= key[i&3]
		}
		source := &streamChunkReader{data: wire, sizes: []int{size}, tail: io.EOF}
		rr := &textPayloadReader{r: bufio.NewReaderSize(source, 16), remaining: int64(len(wire)), masked: true, key: key}
		got, err := io.ReadAll(rr)
		if err != nil || !bytes.Equal(got, payload) || rr.pending != 0 {
			t.Fatalf("chunk=%d err=%v pending=%d", size, err, rr.pending)
		}
	}
}

func TestTextPayloadReaderDoesNotConsumeNextFrame(t *testing.T) {
	for _, size := range []int{1, 511, 512, 513, 65536} {
		payload := bytes.Repeat([]byte{'a'}, size)
		next := []byte{0x81, 1, 'z'}
		src := bytes.NewReader(append(payload, next...))
		br := bufio.NewReader(src)
		r := &textPayloadReader{r: br, remaining: int64(len(payload))}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatal(size, err)
		}
		rest, err := io.ReadAll(br)
		if err != nil || !bytes.Equal(rest, next) {
			t.Fatalf("size=%d next=%x err=%v", size, rest, err)
		}
	}
}

func TestTextPayloadReaderCheckerAllocations(t *testing.T) {
	p := []byte("a¢€𐀀�")
	if n := testing.AllocsPerRun(100, func() {
		var r textPayloadReader
		for _, c := range p {
			q := [1]byte{c}
			if !r.check(q[:]) {
				panic("valid prefix rejected")
			}
		}
	}); n != 0 {
		t.Fatal(n)
	}
}

func TestReadMessageTextNetworkChunks(t *testing.T) {
	boundary := []byte("\x00\x7f\u0080\u07ff\u0800\ud7ff\ue000\ufffd\uffff\U00010000\U0010ffff")
	for _, server := range []bool{false, true} {
		for _, size := range []int{len(boundary), 126, 513, 65536} {
			p := bytes.Repeat([]byte{'a'}, size)
			copy(p, boundary)
			copy(p[len(p)-len(boundary):], boundary)
			for _, chunk := range []int{1, 2, 3, 4, 5, 7, 8, 9, 511, 512, 513} {
				wire := benchmarkWire(p, server, 1)
				wire[0] = 0x81
				wire = append(wire, failureWire(server, 0x81, []byte("next"))...)
				source := &streamChunkReader{data: wire, sizes: []int{chunk}, tail: io.EOF}
				c := NewConn(&benchmarkConn{reader: source, writer: io.Discard}, server, nil)
				c.rw.Reader = bufio.NewReaderSize(source, 16)
				op, got, err := c.ReadMessage()
				if err != nil || op != TextMessage || !bytes.Equal(got, p) {
					t.Fatalf("server=%v size=%d chunk=%d: op=%v err=%v", server, size, chunk, op, err)
				}
				if _, next, err := c.ReadMessage(); err != nil || string(next) != "next" {
					t.Fatalf("next %q %v", next, err)
				}
			}
		}
	}
}

func TestReadMessageInvalidRuneBridgeWithinUnfinishedContinuation(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, prefix := range [][]byte{{0xc2}, {0xe0}, {0xed}, {0xf0}, {0xf4}, {0xf1, 0x80, 0x80}} {
			first := failureWire(server, 0x01, prefix)
			first = append(first, failureWire(server, 0x89, []byte{0xff})...)
			first = append(first, failureWire(server, 0x00, nil)...)
			next := failureWire(server, 0x80, []byte("Xwithheld"))
			h := len(next) - len("Xwithheld")
			raw := &streamChoppedConn{chunks: [][]byte{first, next[:h+1]}, tailErr: streamWithheld}
			c := NewConn(raw, server, nil)
			pings := 0
			c.SetPingHandler(func(p string) error {
				pings++
				if p != "\xff" {
					t.Fatal(p)
				}
				return nil
			})
			if _, _, err := c.ReadMessage(); err != ErrInvalidFrame || raw.tailReads != 0 || pings != 1 {
				t.Fatalf("server=%v prefix=%x err=%v tail=%d pings=%d", server, prefix, err, raw.tailReads, pings)
			}
			if code := binary.BigEndian.Uint16(failureClosePayload(t, raw.written.Bytes(), server)); code != 1007 {
				t.Fatal(code)
			}
		}
	}
}

func TestReadMessageRegisteredExtensionsKeepCompleteFramePath(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			calls := 0
			ext := &headerTransformExtension{enabled: enabled, process: func(f *Frame) error { calls++; f.Payload = []byte("valid"); return nil }}
			payload := []byte{0xff, 'a', 'b', 'c'}
			raw := &streamChoppedConn{chunks: streamParts(server, payload, 1), tailErr: streamWithheld}
			_, _, err := NewConn(raw, server, []Extension{ext}).ReadMessage()
			if !errors.Is(err, streamWithheld) || raw.tailReads != 1 || calls != 0 || raw.written.Len() != 0 {
				t.Fatalf("server=%v enabled=%v err=%v tail=%d calls=%d", server, enabled, err, raw.tailReads, calls)
			}
		}
	}
}

func TestReadMessageTextHugeDeclarationIsIncremental(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, prefix := range [][]byte{nil, {'a'}, {0xf0, 0x90}, {0xff}} {
			h := []byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0, 0}
			binary.BigEndian.PutUint64(h[2:], uint64(^uint(0)>>1))
			p := bytes.Clone(prefix)
			if server {
				h[1] |= 0x80
				key := []byte{1, 2, 3, 4}
				h = append(h, key...)
				for i := range p {
					p[i] ^= key[i&3]
				}
			}
			raw := &streamChoppedConn{chunks: [][]byte{h, p}, tailErr: io.EOF}
			_, _, err := NewConn(raw, server, nil).ReadMessage()
			if len(prefix) > 0 && prefix[0] == 0xff {
				if err != ErrInvalidFrame || raw.tailReads != 0 {
					t.Fatal(err, raw.tailReads)
				}
			} else if !errors.Is(err, io.ErrUnexpectedEOF) || raw.written.Len() != 0 {
				t.Fatal(err)
			}
		}
	}
}

// Differentially check the integrated network-chunk path, independently of the
// production prefix helper. Masking comes from the scalar wire fixture.
func FuzzUTF8NetworkChunks(f *testing.F) {
	for _, p := range [][]byte{nil, []byte("a¢€𐀀�"), {0xf4, 0x90}, {0xe0, 0xa0}, {0xff}} {
		f.Add(p, byte(1), false)
		f.Add(p, byte(3), true)
	}
	f.Fuzz(func(t *testing.T, p []byte, chunk byte, server bool) {
		if len(p) > 8192 {
			t.Skip()
		}
		wire := benchmarkWire(p, server, 1)
		wire[0] = 0x81
		src := &streamChunkReader{data: wire, sizes: []int{1 + int(chunk)}, tail: io.EOF}
		c := NewConn(&benchmarkConn{reader: src, writer: io.Discard}, server, nil)
		c.rw.Reader = bufio.NewReaderSize(src, 16)
		op, got, err := c.ReadMessage()
		if utf8.Valid(p) {
			if err != nil || op != TextMessage || !bytes.Equal(got, p) {
				t.Fatalf("valid %x got=%x op=%v err=%v", p, got, op, err)
			}
		} else if err != ErrInvalidFrame || got != nil || op != 0 {
			t.Fatalf("invalid %x got=%x op=%v err=%v", p, got, op, err)
		}
	})
}
