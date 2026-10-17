package deflate

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

// windowInflater is a small, independent RFC 1951 decoder used only by tests.
// Unlike compress/flate it rejects any back-reference beyond its window, so it
// proves the encoder honors a negotiated window size.
type windowInflater struct {
	in     []byte
	bitPos int
	window int
	out    []byte
}

var errTooFar = errors.New("distance exceeds window")

func (d *windowInflater) bits(n int) (int, error) {
	v := 0
	for i := range n {
		if d.bitPos>>3 >= len(d.in) {
			return 0, io.ErrUnexpectedEOF
		}
		v |= int(d.in[d.bitPos>>3]>>(d.bitPos&7)&1) << i
		d.bitPos++
	}
	return v, nil
}

type testHuffman struct {
	count  [16]int
	symbol []int
}

func newTestHuffman(lengths []uint8) *testHuffman {
	h := &testHuffman{}
	for _, l := range lengths {
		h.count[l]++
	}
	h.count[0] = 0
	var offs [16]int
	for l := 1; l < 15; l++ {
		offs[l+1] = offs[l] + h.count[l]
	}
	h.symbol = make([]int, len(lengths))
	for sym, l := range lengths {
		if l != 0 {
			h.symbol[offs[l]] = sym
			offs[l]++
		}
	}
	return h
}

// decode follows the canonical decoding loop from zlib's puff.c.
func (d *windowInflater) decode(h *testHuffman) (int, error) {
	code, first, index := 0, 0, 0
	for l := 1; l < 16; l++ {
		b, err := d.bits(1)
		if err != nil {
			return 0, err
		}
		code |= b
		count := h.count[l]
		if code-count < first {
			return h.symbol[index+(code-first)], nil
		}
		index += count
		first += count
		first <<= 1
		code <<= 1
	}
	return 0, errors.New("invalid code")
}

// inflate decodes non-final blocks until the input is exhausted at a byte
// boundary after a stored block, the shape a sync flush produces.
func (d *windowInflater) inflate() error {
	for d.bitPos>>3 < len(d.in) {
		final, err := d.bits(1)
		if err != nil {
			return err
		}
		if final != 0 {
			return errors.New("unexpected final block")
		}
		kind, err := d.bits(2)
		if err != nil {
			return err
		}
		switch kind {
		case 0:
			d.bitPos = (d.bitPos + 7) &^ 7
			i := d.bitPos >> 3
			if i+4 > len(d.in) {
				return io.ErrUnexpectedEOF
			}
			n := int(d.in[i]) | int(d.in[i+1])<<8
			if nn := int(d.in[i+2]) | int(d.in[i+3])<<8; n != ^nn&0xffff {
				return errors.New("stored length mismatch")
			}
			if i+4+n > len(d.in) {
				return io.ErrUnexpectedEOF
			}
			d.out = append(d.out, d.in[i+4:i+4+n]...)
			d.bitPos = (i + 4 + n) << 3
		case 1:
			if err := d.codes(&fixedLitLen, &fixedDist); err != nil {
				return err
			}
		case 2:
			if err := d.dynamic(); err != nil {
				return err
			}
		default:
			return errors.New("reserved block type")
		}
	}
	return nil
}

func (d *windowInflater) dynamic() error {
	nlen, err := d.bits(5)
	if err != nil {
		return err
	}
	ndist, _ := d.bits(5)
	ncode, _ := d.bits(4)
	nlen, ndist, ncode = nlen+257, ndist+1, ncode+4
	var cl [numCodeLen]uint8
	for _, sym := range codeLenOrder[:ncode] {
		v, err := d.bits(3)
		if err != nil {
			return err
		}
		cl[sym] = uint8(v)
	}
	clh := newTestHuffman(cl[:])
	lengths := make([]uint8, 0, nlen+ndist)
	for len(lengths) < nlen+ndist {
		sym, err := d.decode(clh)
		if err != nil {
			return err
		}
		switch {
		case sym < 16:
			lengths = append(lengths, uint8(sym))
		case sym == 16:
			if len(lengths) == 0 {
				return errors.New("repeat without previous length")
			}
			n, _ := d.bits(2)
			for range n + 3 {
				lengths = append(lengths, lengths[len(lengths)-1])
			}
		case sym == 17:
			n, _ := d.bits(3)
			lengths = append(lengths, make([]uint8, n+3)...)
		default:
			n, _ := d.bits(7)
			lengths = append(lengths, make([]uint8, n+11)...)
		}
	}
	if len(lengths) != nlen+ndist {
		return errors.New("code lengths overrun")
	}
	var lit, dist huffmanCode
	lit.assign(lengths[:nlen])
	dist.assign(lengths[nlen:])
	return d.codes(&lit, &dist)
}

func (d *windowInflater) codes(lit, dist *huffmanCode) error {
	lh, dh := newTestHuffman(lit.length), newTestHuffman(dist.length)
	for {
		sym, err := d.decode(lh)
		if err != nil {
			return err
		}
		if sym < 256 {
			d.out = append(d.out, byte(sym))
			continue
		}
		if sym == endOfBlock {
			return nil
		}
		sym -= 257
		if sym >= 29 {
			return errors.New("invalid length symbol")
		}
		extra, err := d.bits(int(lengthExtra[sym]))
		if err != nil {
			return err
		}
		length := int(lengthBase[sym]) + extra
		ds, err := d.decode(dh)
		if err != nil {
			return err
		}
		if ds >= numDist {
			return errors.New("invalid distance symbol")
		}
		extra, err = d.bits(int(distExtra[ds]))
		if err != nil {
			return err
		}
		distance := int(distBase[ds]) + extra
		if distance > d.window {
			return fmt.Errorf("%w: %d > %d", errTooFar, distance, d.window)
		}
		if distance > len(d.out) {
			return errors.New("distance before start of history")
		}
		for range length {
			d.out = append(d.out, d.out[len(d.out)-distance])
		}
	}
}

func testInputs() map[string][]byte {
	r := rand.New(rand.NewPCG(1, 2))
	random := make([]byte, 200_000)
	for i := range random {
		random[i] = byte(r.Uint32())
	}
	text := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 4000))
	mixed := make([]byte, 0, 300_000)
	for len(mixed) < 300_000 {
		switch r.IntN(3) {
		case 0:
			mixed = append(mixed, random[:r.IntN(500)]...)
		case 1:
			mixed = append(mixed, text[:r.IntN(2000)]...)
		default:
			mixed = append(mixed, bytes.Repeat([]byte{byte(r.Uint32())}, r.IntN(1000))...)
		}
	}
	words := make([]byte, 0, 200_000)
	vocab := strings.Fields("websocket deflate frame message window context takeover compress json { } [ ] , : \" id name value true false null 0 1 42 3.14")
	for len(words) < 200_000 {
		words = append(words, vocab[r.IntN(len(vocab))]...)
		words = append(words, ' ')
	}
	return map[string][]byte{
		"empty":  nil,
		"byte":   {'x'},
		"short":  []byte("hello"),
		"zeros":  make([]byte, 100_000),
		"random": random,
		"text":   text,
		"mixed":  mixed,
		"words":  words,
	}
}

// checkStream decodes compressed with both inflaters and compares the result.
func checkStream(t *testing.T, windowBits int, dict, compressed, want []byte) {
	t.Helper()
	d := &windowInflater{in: compressed, window: 1 << windowBits, out: bytes.Clone(dict)}
	if err := d.inflate(); err != nil {
		t.Fatalf("window inflater: %v", err)
	}
	if got := d.out[len(dict):]; !bytes.Equal(got, want) {
		t.Fatalf("window inflater output mismatch: got %d bytes, want %d", len(got), len(want))
	}
	r := flate.NewReaderDict(io.MultiReader(bytes.NewReader(compressed), bytes.NewReader([]byte{1, 0, 0, 0xff, 0xff})), dict)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("compress/flate: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("compress/flate output mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func TestEncoderRoundTrip(t *testing.T) {
	for name, input := range testInputs() {
		for _, wb := range []int{8, 9, 10, 12, 15} {
			for _, level := range []int{HuffmanOnly, 0, 1, 3, 4, 6, 9} {
				t.Run(fmt.Sprintf("%s/w%d/l%d", name, wb, level), func(t *testing.T) {
					e, err := NewEncoder(wb, level)
					if err != nil {
						t.Fatal(err)
					}
					out := e.Flush(e.Write(nil, input))
					if !bytes.HasSuffix(out, []byte{0, 0, 0xff, 0xff}) {
						t.Fatalf("missing sync flush marker")
					}
					checkStream(t, wb, nil, out, input)
				})
			}
		}
	}
}

func TestEncoderCompresses(t *testing.T) {
	in := testInputs()
	for _, name := range []string{"text", "zeros", "words"} {
		e, _ := NewEncoder(15, DefaultCompression)
		out := e.Flush(e.Write(nil, in[name]))
		var ref bytes.Buffer
		w, _ := flate.NewWriter(&ref, flate.DefaultCompression)
		w.Write(in[name])
		w.Flush()
		t.Logf("%s: %d -> %d (compress/flate %d)", name, len(in[name]), len(out), ref.Len())
		if len(out) > ref.Len()*11/10+16 {
			t.Errorf("%s: compressed to %d bytes, more than 10%% above compress/flate's %d", name, len(out), ref.Len())
		}
	}
	e, _ := NewEncoder(15, BestSpeed)
	random := in["random"]
	if out := e.Flush(e.Write(nil, random)); len(out) > len(random)+len(random)/1000+64 {
		t.Errorf("random input expanded to %d bytes", len(out))
	}
}

// TestEncoderStreaming covers small writes, repeated sync flushes and
// context takeover between flushes through History and Reset.
func TestEncoderStreaming(t *testing.T) {
	input := testInputs()["mixed"]
	r := rand.New(rand.NewPCG(3, 4))
	for _, wb := range []int{8, 11, 15} {
		for _, level := range []int{1, 6, 9} {
			t.Run(fmt.Sprintf("w%d/l%d", wb, level), func(t *testing.T) {
				e, _ := NewEncoder(wb, level)
				var history []byte
				rest := input
				for len(rest) > 0 {
					msg := rest[:min(len(rest), 1+r.IntN(20_000))]
					rest = rest[len(msg):]
					var out []byte
					for p := msg; len(p) > 0; {
						n := min(len(p), 1+r.IntN(3000))
						out = e.Write(out, p[:n])
						p = p[n:]
					}
					out = e.Flush(out)
					checkStream(t, wb, history, out, msg)
					history = append(history, msg...)
					if r.IntN(3) == 0 {
						// Continue on a fresh encoder from the saved window only.
						h := e.History(nil)
						e, _ = NewEncoder(wb, level)
						e.Reset(h)
					}
				}
			})
		}
	}
}

func TestEncoderEmptyFlush(t *testing.T) {
	e, _ := NewEncoder(15, DefaultCompression)
	if out := e.Flush(nil); !bytes.Equal(out, []byte{0, 0, 0, 0xff, 0xff}) {
		t.Fatalf("empty flush = %x, want 000000ffff", out)
	}
	// RFC 7692 section 7.2.3.6: removing the tail leaves the single octet 0x00.
	out := e.Flush(e.Write(nil, []byte("abc")))
	if out = e.Flush(nil); !bytes.Equal(out, []byte{0, 0, 0, 0xff, 0xff}) {
		t.Fatalf("second empty flush = %x", out)
	}
}

func TestNewEncoderRejectsInvalidParameters(t *testing.T) {
	for _, tc := range [][2]int{{7, 6}, {16, 6}, {15, 10}, {15, -3}} {
		if _, err := NewEncoder(tc[0], tc[1]); !errors.Is(err, ErrInvalidParameter) {
			t.Errorf("NewEncoder(%d, %d) error = %v", tc[0], tc[1], err)
		}
	}
}

func TestEncoderSteadyStateAllocations(t *testing.T) {
	e, _ := NewEncoder(15, DefaultCompression)
	msg := testInputs()["words"][:8192]
	dst := make([]byte, 0, 64<<10)
	allocs := testing.AllocsPerRun(50, func() {
		dst = e.Flush(e.Write(dst[:0], msg))
	})
	if allocs != 0 {
		t.Fatalf("allocations per message = %v, want 0", allocs)
	}
}

func FuzzEncoder(f *testing.F) {
	f.Add([]byte("hello hello hello"), uint8(15), int8(6), uint16(7))
	f.Add(bytes.Repeat([]byte{0}, 1000), uint8(8), int8(1), uint16(100))
	f.Fuzz(func(t *testing.T, data []byte, wb uint8, level int8, chunk uint16) {
		windowBits := MinWindowBits + int(wb)%(MaxWindowBits-MinWindowBits+1)
		lvl := HuffmanOnly + (int(level)+128)%(BestCompression-HuffmanOnly+1)
		e, err := NewEncoder(windowBits, lvl)
		if err != nil {
			t.Fatal(err)
		}
		n := max(1, int(chunk))
		var history, out []byte
		for i, p := 0, data; len(p) > 0; i++ {
			k := min(n, len(p))
			out = e.Flush(e.Write(nil, p[:k]))
			checkStream(t, windowBits, history, out, p[:k])
			history = append(history, p[:k]...)
			p = p[k:]
		}
		_ = out
	})
}

func BenchmarkEncoder(b *testing.B) {
	in := testInputs()
	for _, name := range []string{"text", "words", "random"} {
		for _, level := range []int{1, 6} {
			b.Run(fmt.Sprintf("%s/l%d/deflate", name, level), func(b *testing.B) {
				e, _ := NewEncoder(15, level)
				dst := make([]byte, 0, len(in[name])*2)
				b.SetBytes(int64(len(in[name])))
				b.ReportAllocs()
				for b.Loop() {
					e.Reset(nil)
					dst = e.Flush(e.Write(dst[:0], in[name]))
				}
				b.ReportMetric(float64(len(dst))/float64(len(in[name])), "ratio")
			})
			b.Run(fmt.Sprintf("%s/l%d/stdlib", name, level), func(b *testing.B) {
				var buf bytes.Buffer
				w, _ := flate.NewWriter(&buf, level)
				b.SetBytes(int64(len(in[name])))
				b.ReportAllocs()
				for b.Loop() {
					buf.Reset()
					w.Reset(&buf)
					w.Write(in[name])
					w.Flush()
				}
				b.ReportMetric(float64(buf.Len())/float64(len(in[name])), "ratio")
			})
		}
	}
}
