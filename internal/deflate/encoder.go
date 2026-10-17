// Package deflate implements a raw DEFLATE (RFC 1951) encoder whose LZ77
// back-references never reach further than a configured window.
//
// The standard library's compress/flate always uses a 32 KiB window and cannot
// be reset with a new preset dictionary without allocating a new writer. The
// WebSocket permessage-deflate extension (RFC 7692) needs both: a peer may limit
// the sender's window to as little as 256 bytes (section 7.1.2), and context
// takeover (section 7.1.1) carries history from one message to the next. This
// encoder supports window sizes from 2^8 to 2^15 bytes, sync flushes, and cheap
// [Encoder.Reset] with a dictionary so callers can keep only the history bytes
// between messages and pool the much larger working state.
//
// Output is decodable by any conforming inflater, including compress/flate.
//
// https://www.rfc-editor.org/rfc/rfc1951
// https://www.rfc-editor.org/rfc/rfc7692#section-7.1.2
package deflate

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
)

// Window size limits in bits. RFC 7692 section 7.1.2.1 allows 8 through 15.
const (
	MinWindowBits = 8
	MaxWindowBits = 15
)

// Compression levels follow compress/flate's numbering.
const (
	BestSpeed          = 1
	BestCompression    = 9
	DefaultCompression = -1
	// HuffmanOnly disables LZ77 matching and only entropy-codes literals.
	HuffmanOnly = -2
)

// ErrInvalidParameter reports an unsupported window size or level.
var ErrInvalidParameter = errors.New("deflate: invalid window bits or compression level")

const (
	hashBitsMax = 16
	hashLen     = 4       // Bytes hashed per position; shorter matches are not searched.
	blockBytes  = 1 << 16 // Raw bytes accumulated in the window per block.
	maxTokens   = 1 << 14 // Tokens buffered before a block is emitted.
	matchFlag   = 1 << 31
	rebaseLimit = 1 << 30 // Largest stream offset before hash entries are rebased.

	// A Reset dictionary is indexed densely only in its final dictDense
	// bytes, and at every dictStride-th position before that. A long match
	// into the older part is still found within dictStride bytes of where it
	// starts, at a quarter of the indexing cost per message.
	dictDense    = 4 << 10
	dictStride   = 4
	lengthShift  = 16
	distanceMask = 1<<lengthShift - 1
)

// levelParams tunes the matcher. The shape follows zlib's configuration_table;
// chains are shorter because this encoder favors predictable latency.
type levelParams struct {
	good, lazy, nice, chain int
}

var levels = [10]levelParams{
	1: {4, 0, 16, 1},
	2: {4, 0, 32, 4},
	3: {4, 0, 64, 8},
	4: {4, 4, 16, 8},
	5: {8, 16, 32, 16},
	6: {8, 16, 64, 32},
	7: {8, 32, 128, 128},
	8: {32, 128, 258, 512},
	9: {32, 258, 258, 4096},
}

// Encoder is a streaming raw DEFLATE compressor with a bounded LZ77 window.
// It never emits a final block: output is a sequence of non-final blocks
// terminated by sync flushes, as RFC 7692 section 7.2.1 requires.
//
// An Encoder is not safe for concurrent use.
type Encoder struct {
	windowBits int
	level      int
	params     levelParams
	window     int // 1 << windowBits; the maximum back-reference distance.
	hashBits   uint

	// win holds up to window bytes of history followed by pending input.
	// head and prev hold stream offsets: win[i] is at offset base+i. Entries
	// below base are stale, either slid out of win or left by an earlier
	// stream, so neither slide nor Reset rewrites the tables, and prev stays
	// indexed by offset & (window-1) across slides.
	win   []byte
	base  int
	pos   int // Next position to tokenize.
	end   int // End of buffered input.
	block int // Start of the bytes the buffered tokens describe.
	cover int // End of the bytes the buffered tokens describe.
	head  []int32
	prev  []int32 // Indexed by offset & (window-1).
	ins   int     // Positions below ins have been inserted into the hash chains.

	// Lazy-match state carried across Write calls.
	prevLen, prevDist int
	pending           bool

	tokens []uint32

	litFreq  [numLitLen]uint32
	distFreq [numDist]uint32
	litLen   huffmanCode
	dist     huffmanCode
	codeLen  huffmanCode
	builder  huffmanBuilder
	lens     [numLitLen + numDist]uint8
	rle      []uint8
	seq      []uint8

	out   []byte
	bits  uint64
	nbits uint
}

// NewEncoder returns an encoder for windowBits (8-15) at level (-2 to 9).
func NewEncoder(windowBits, level int) (*Encoder, error) {
	if windowBits < MinWindowBits || windowBits > MaxWindowBits {
		return nil, ErrInvalidParameter
	}
	if level == DefaultCompression {
		level = 6
	}
	if level < HuffmanOnly || level > BestCompression {
		return nil, ErrInvalidParameter
	}
	e := &Encoder{windowBits: windowBits, level: level, window: 1 << windowBits}
	if level > 0 {
		e.params = levels[level]
		e.hashBits = uint(min(hashBitsMax, windowBits+1))
		e.head = make([]int32, 1<<e.hashBits)
		e.prev = make([]int32, e.window)
	}
	e.win = make([]byte, e.window+blockBytes)
	e.tokens = make([]uint32, 0, maxTokens)
	e.base = 1 // Zeroed table entries are stale.
	e.Reset(nil)
	return e, nil
}

// WindowBits reports the configured window size in bits.
func (e *Encoder) WindowBits() int { return e.windowBits }

// Level reports the configured compression level.
func (e *Encoder) Level() int { return e.level }

// Reset discards all state and starts a new stream whose back-references may
// reach into the final window bytes of dict. The caller must ensure the
// decoder's history ends with the same bytes, as context takeover does.
//
// Reset does not clear the encoder's tables; its cost is indexing dict. Only
// the final 4 KiB is indexed at every position, so a match that starts
// further back may be found a few bytes late.
func (e *Encoder) Reset(dict []byte) {
	if len(dict) > e.window {
		dict = dict[len(dict)-e.window:]
	}
	// Start past every offset of the previous stream, invalidating its hash
	// entries without touching the tables.
	e.base += e.end
	if e.base > rebaseLimit {
		e.rebase()
	}
	e.litFreq, e.distFreq = [numLitLen]uint32{}, [numDist]uint32{}
	e.end = copy(e.win, dict)
	e.pos, e.block, e.cover, e.ins = e.end, e.end, e.end, 0
	e.prevLen, e.prevDist, e.pending = 0, 0, false
	e.tokens = e.tokens[:0]
	e.bits, e.nbits = 0, 0
	if e.level > 0 {
		mask := e.window - 1
		for i := 0; i < e.end-dictDense; i += dictStride {
			h, off := e.hash(i), e.base+i
			e.prev[off&mask] = e.head[h]
			e.head[h] = int32(off)
		}
		e.ins = max(0, e.end-dictDense)
		e.insert(e.end)
	}
}

// History appends the final window bytes of input seen so far (including any
// Reset dictionary) to dst. Pass the result to Reset to continue the stream's
// context on another encoder after a sync flush.
func (e *Encoder) History(dst []byte) []byte {
	return append(dst, e.win[max(0, e.end-e.window):e.end]...)
}

// Write compresses p and appends any completed output to dst. Output is not
// complete until Flush.
func (e *Encoder) Write(dst, p []byte) []byte {
	e.out = dst
	for len(p) > 0 {
		if e.end == len(e.win) {
			e.compress(false)
			e.slide()
		}
		n := copy(e.win[e.end:], p)
		e.end += n
		p = p[n:]
		if e.end-e.pos >= maxMatch+blockBytes/2 || e.end == len(e.win) {
			e.compress(false)
		}
	}
	dst, e.out = e.out, nil
	return dst
}

// Flush compresses all pending input, then appends a sync flush: an empty
// stored block ending in 0x00 0x00 0xff 0xff (RFC 1951 section 3.2.4). The
// output is byte-aligned and the stream may continue with further writes.
func (e *Encoder) Flush(dst []byte) []byte {
	e.out = dst
	e.compress(true)
	e.writeBlock()
	e.writeBits(0, 3) // BFINAL=0, BTYPE=00.
	e.alignByte()
	e.out = append(e.out, 0, 0, 0xff, 0xff)
	dst, e.out = e.out, nil
	return dst
}

// slide drops input older than the window to make room for more input.
// All tokens must already have been written.
func (e *Encoder) slide() {
	e.writeBlock()
	delta := e.pos - e.window
	if delta <= 0 {
		return
	}
	copy(e.win, e.win[delta:e.end])
	e.pos -= delta
	e.end -= delta
	e.block -= delta
	e.cover -= delta
	e.ins = max(0, e.ins-delta)
	e.base += delta
	if e.base > rebaseLimit {
		e.rebase()
	}
}

// rebase lowers base, and every hash entry with it, by a multiple of the
// window, so offsets fit in int32 and prev slots keep their alignment.
// Stale entries clamp to 0, which stays below the new base.
func (e *Encoder) rebase() {
	shift := (e.base - 1) &^ (e.window - 1)
	e.base -= shift
	for _, t := range [2][]int32{e.head, e.prev} {
		for i, v := range t {
			t[i] = int32(max(int(v)-shift, 0))
		}
	}
}

func (e *Encoder) hash(i int) uint32 {
	return (binary.LittleEndian.Uint32(e.win[i:]) * 0x9e3779b1) >> (32 - e.hashBits)
}

// insert adds hash-chain entries for positions [e.ins, upTo).
func (e *Encoder) insert(upTo int) {
	upTo = min(upTo, e.end-hashLen+1)
	mask := e.window - 1
	for i := e.ins; i < upTo; i++ {
		h, off := e.hash(i), e.base+i
		e.prev[off&mask] = e.head[h]
		e.head[h] = int32(off)
	}
	e.ins = max(e.ins, upTo)
}

// findMatch returns the longest match at pos that is longer than best, with a
// distance of at most the window size.
func (e *Encoder) findMatch(pos, best int) (length, dist int) {
	avail := min(maxMatch, e.end-pos)
	if avail < hashLen || best >= avail {
		return 0, 0
	}
	chain := e.params.chain
	if best >= e.params.good {
		chain >>= 2
	}
	nice := min(e.params.nice, avail)
	// Candidates are stream offsets; win[off-base] holds offset off.
	base, off := e.base, e.base+pos
	limit := max(base, off-e.window)
	mask := e.window - 1
	win := e.win
	cand := int(e.head[e.hash(pos)])
	// The current position is usually inserted already; skip a self-reference.
	if cand == off {
		cand = int(e.prev[cand&mask])
	}
	first := binary.LittleEndian.Uint32(win[pos:])
	for ; cand >= limit && cand < off && chain > 0; chain-- {
		i := cand - base
		if win[i+best] == win[pos+best] && binary.LittleEndian.Uint32(win[i:]) == first {
			n := matchLen(win[i:i+avail], win[pos:pos+avail])
			if n > best {
				best, length, dist = n, n, off-cand
				if n >= nice {
					break
				}
			}
		}
		next := int(e.prev[cand&mask])
		if next >= cand {
			break
		}
		cand = next
	}
	return length, dist
}

// matchLen returns the length of the common prefix of a and b (len(a) == len(b)).
func matchLen(a, b []byte) int {
	n := 0
	for len(a)-n >= 8 {
		x := binary.LittleEndian.Uint64(a[n:]) ^ binary.LittleEndian.Uint64(b[n:])
		if x != 0 {
			return n + bits.TrailingZeros64(x)>>3
		}
		n += 8
	}
	for n < len(a) && a[n] == b[n] {
		n++
	}
	return n
}

// compress tokenizes buffered input. Without final it keeps maxMatch bytes of
// lookahead so matches are never cut short by a Write boundary.
func (e *Encoder) compress(final bool) {
	stop := e.end
	if !final {
		stop -= maxMatch
	}
	if e.level <= 0 {
		for ; e.pos < stop; e.pos++ {
			e.literal(e.win[e.pos])
		}
		return
	}
	if e.params.lazy == 0 {
		// Greedy matching. Long literal runs probe progressively sparser
		// positions, so incompressible input is not searched byte by byte.
		misses := 0
		for e.pos < stop {
			e.ins = max(e.ins, e.pos)
			e.insert(e.pos + 1)
			length, dist := e.findMatch(e.pos, minMatch-1)
			if length == 0 {
				misses++
				for n := 1 + misses>>5; n > 0 && e.pos < stop; n-- {
					e.literal(e.win[e.pos])
					e.pos++
				}
				continue
			}
			misses = 0
			e.match(length, dist)
			// Index only the tail of long matches; their interior rarely
			// starts a better match and dominates time on repetitive input.
			if length > e.params.nice {
				e.ins = max(e.ins, e.pos+length-hashLen)
			}
			e.pos += length
			e.insert(e.pos)
		}
		return
	}
	misses := 0
	for e.pos < stop {
		e.ins = max(e.ins, e.pos)
		e.insert(e.pos + 1)
		length, dist := 0, 0
		if e.prevLen < e.params.lazy {
			length, dist = e.findMatch(e.pos, max(e.prevLen, minMatch-1))
		}
		if e.pending && e.prevLen >= minMatch && length <= e.prevLen {
			// The previous position's match is at least as good: take it.
			e.match(e.prevLen, e.prevDist)
			e.pos += e.prevLen - 1
			e.insert(e.pos)
			e.pending, e.prevLen, misses = false, 0, 0
			continue
		}
		if e.pending {
			e.literal(e.win[e.pos-1])
		}
		if length == 0 {
			// As in the greedy path, a long literal run skips probing (and
			// indexing) progressively more positions.
			if misses++; misses >= 32 {
				for n := 1 + misses>>5; n > 0 && e.pos < stop; n-- {
					e.literal(e.win[e.pos])
					e.pos++
				}
				e.pending, e.prevLen = false, 0
				continue
			}
		} else {
			misses = 0
		}
		e.pending, e.prevLen, e.prevDist = true, length, dist
		e.pos++
	}
	if final && e.pending {
		if e.prevLen >= minMatch {
			e.match(e.prevLen, e.prevDist)
			e.pos += e.prevLen - 1
			e.insert(e.pos)
		} else {
			e.literal(e.win[e.pos-1])
		}
		e.pending, e.prevLen = false, 0
	}
}

func (e *Encoder) literal(b byte) {
	e.tokens = append(e.tokens, uint32(b))
	e.litFreq[b]++
	e.cover++
	if len(e.tokens) == maxTokens {
		e.writeBlock()
	}
}

func (e *Encoder) match(length, dist int) {
	e.tokens = append(e.tokens, matchFlag|uint32(length-minMatch)<<lengthShift|uint32(dist-1))
	e.litFreq[257+int(lengthCode[length-minMatch])]++
	e.distFreq[distCode(dist)]++
	e.cover += length
	if len(e.tokens) == maxTokens {
		e.writeBlock()
	}
}

// writeBlock emits buffered tokens as the smallest of a stored, fixed or
// dynamic Huffman block (RFC 1951 section 3.2.3).
func (e *Encoder) writeBlock() {
	if len(e.tokens) == 0 {
		return
	}
	raw := e.win[e.block:e.cover]
	e.litFreq[endOfBlock]++

	litLens, distLens := e.lens[:numLitLen], e.lens[numLitLen:]
	e.builder.build(e.litFreq[:], litLens, maxCodeBits)
	e.builder.build(e.distFreq[:], distLens, maxCodeBits)
	nlit, ndist := numLitLen, numDist
	for nlit > 257 && litLens[nlit-1] == 0 {
		nlit--
	}
	for ndist > 1 && distLens[ndist-1] == 0 {
		ndist--
	}
	e.litLen.assign(litLens[:nlit])
	e.dist.assign(distLens[:ndist])

	extra := 0
	for code, f := range e.litFreq[257:] {
		extra += int(f) * int(lengthExtra[code])
	}
	for code, f := range e.distFreq {
		extra += int(f) * int(distExtra[code])
	}

	// Code-length sequence with run-length codes 16-18 (section 3.2.7).
	var clFreq [numCodeLen]uint32
	e.rle = e.runLengths(e.rle[:0], litLens[:nlit], distLens[:ndist], &clFreq)
	var clLens [numCodeLen]uint8
	e.builder.build(clFreq[:], clLens[:], maxCLenBits)
	e.codeLen.assign(clLens[:])
	nclen := numCodeLen
	for nclen > 4 && clLens[codeLenOrder[nclen-1]] == 0 {
		nclen--
	}
	dynamicBits := 3 + 5 + 5 + 4 + 3*nclen + extra +
		e.litLen.bitLength(e.litFreq[:nlit]) + e.dist.bitLength(e.distFreq[:ndist])
	for i := 0; i < len(e.rle); i++ {
		sym := e.rle[i]
		dynamicBits += int(clLens[sym])
		switch sym {
		case 16:
			dynamicBits += 2
			i++
		case 17:
			dynamicBits += 3
			i++
		case 18:
			dynamicBits += 7
			i++
		}
	}
	fixedBits := 3 + extra + fixedLitLen.bitLength(e.litFreq[:]) + fixedDist.bitLength(e.distFreq[:])
	chunks := (len(raw) + 0xfffe) / 0xffff
	storedBits := ((int(e.nbits)+3+7)&^7 - int(e.nbits)) + 8*(chunks-1) + 32*chunks + 8*len(raw)
	if len(raw) == 0 {
		storedBits = math.MaxInt
	} else if e.level == 0 {
		storedBits = 0
	}

	switch {
	case storedBits <= fixedBits && storedBits <= dynamicBits:
		e.writeStored(raw)
	case fixedBits <= dynamicBits:
		e.writeBits(1<<1, 3) // BFINAL=0, BTYPE=01.
		e.writeTokens(&fixedLitLen, &fixedDist)
	default:
		e.writeBits(2<<1, 3) // BFINAL=0, BTYPE=10.
		e.writeBits(uint64(nlit-257), 5)
		e.writeBits(uint64(ndist-1), 5)
		e.writeBits(uint64(nclen-4), 4)
		for _, sym := range codeLenOrder[:nclen] {
			e.writeBits(uint64(clLens[sym]), 3)
		}
		for i := 0; i < len(e.rle); i++ {
			sym := e.rle[i]
			e.writeBits(uint64(e.codeLen.code[sym]), uint(clLens[sym]))
			switch sym {
			case 16:
				i++
				e.writeBits(uint64(e.rle[i]), 2)
			case 17:
				i++
				e.writeBits(uint64(e.rle[i]), 3)
			case 18:
				i++
				e.writeBits(uint64(e.rle[i]), 7)
			}
		}
		e.writeTokens(&e.litLen, &e.dist)
	}
	e.tokens = e.tokens[:0]
	clear(e.litFreq[:])
	clear(e.distFreq[:])
	e.block = e.cover
}

// runLengths appends the code-length symbols for lit and dist to dst. Repeat
// symbols are followed by their extra-bit value in the same slice.
func (e *Encoder) runLengths(dst, lit, dist []uint8, freq *[numCodeLen]uint32) []uint8 {
	e.seq = append(append(e.seq[:0], lit...), dist...)
	all := e.seq
	for i := 0; i < len(all); {
		l := all[i]
		run := 1
		for i+run < len(all) && all[i+run] == l {
			run++
		}
		i += run
		if l == 0 {
			for run >= 11 {
				n := min(run, 138)
				dst = append(dst, 18, uint8(n-11))
				freq[18]++
				run -= n
			}
			if run >= 3 {
				dst = append(dst, 17, uint8(run-3))
				freq[17]++
				run = 0
			}
		} else if run >= 4 {
			dst = append(dst, l)
			freq[l]++
			run--
			for run >= 3 {
				n := min(run, 6)
				dst = append(dst, 16, uint8(n-3))
				freq[16]++
				run -= n
			}
		}
		for ; run > 0; run-- {
			dst = append(dst, l)
			freq[l]++
		}
	}
	return dst
}

func (e *Encoder) writeTokens(lit, dist *huffmanCode) {
	for _, t := range e.tokens {
		if t&matchFlag == 0 {
			e.writeBits(uint64(lit.code[t]), uint(lit.length[t]))
			continue
		}
		length := int(t>>lengthShift&0xff) + minMatch
		d := int(t&distanceMask) + 1
		lc := lengthCode[length-minMatch]
		sym := 257 + int(lc)
		e.writeBits(uint64(lit.code[sym]), uint(lit.length[sym]))
		if n := lengthExtra[lc]; n > 0 {
			e.writeBits(uint64(length-int(lengthBase[lc])), uint(n))
		}
		dc := distCode(d)
		e.writeBits(uint64(dist.code[dc]), uint(dist.length[dc]))
		if n := distExtra[dc]; n > 0 {
			e.writeBits(uint64(d-int(distBase[dc])), uint(n))
		}
	}
	e.writeBits(uint64(lit.code[endOfBlock]), uint(lit.length[endOfBlock]))
}

// writeStored emits raw as stored blocks of at most 65535 bytes (section 3.2.4).
func (e *Encoder) writeStored(raw []byte) {
	for {
		n := min(len(raw), 0xffff)
		e.writeBits(0, 3) // BFINAL=0, BTYPE=00.
		e.alignByte()
		e.out = append(e.out, byte(n), byte(n>>8), ^byte(n), ^byte(n>>8))
		e.out = append(e.out, raw[:n]...)
		raw = raw[n:]
		if len(raw) == 0 {
			return
		}
	}
}

func (e *Encoder) writeBits(v uint64, n uint) {
	e.bits |= v << e.nbits
	e.nbits += n
	for e.nbits >= 32 {
		e.out = binary.LittleEndian.AppendUint32(e.out, uint32(e.bits))
		e.bits >>= 32
		e.nbits -= 32
	}
}

func (e *Encoder) alignByte() {
	for e.nbits > 0 {
		e.out = append(e.out, byte(e.bits))
		e.bits >>= 8
		e.nbits = e.nbits - min(e.nbits, 8)
	}
	e.bits = 0
}
