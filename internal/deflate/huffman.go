package deflate

import (
	"cmp"
	"math/bits"
	"slices"
)

// Alphabet sizes and code-length limits from RFC 1951 section 3.2.5-3.2.7.
//
// https://www.rfc-editor.org/rfc/rfc1951#section-3.2.5
const (
	numLitLen   = 286 // Literal/length symbols that may appear in a block.
	numDist     = 30  // Distance symbols that may appear in a block.
	numCodeLen  = 19  // Code-length alphabet for dynamic block headers.
	maxCodeBits = 15  // Longest literal/length or distance code.
	maxCLenBits = 7   // Longest code-length code.
	endOfBlock  = 256 // Literal/length symbol that terminates a block.
	minMatch    = 3   // Shortest LZ77 match DEFLATE can encode.
	maxMatch    = 258 // Longest LZ77 match DEFLATE can encode.
)

// codeLenOrder is the transmission order of code-length code lengths.
//
// https://www.rfc-editor.org/rfc/rfc1951#section-3.2.7
var codeLenOrder = [numCodeLen]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// lengthBase and lengthExtra describe symbols 257-285 (RFC 1951 section 3.2.5).
var lengthBase = [29]uint16{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
var lengthExtra = [29]uint8{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}

// distBase and distExtra describe distance symbols 0-29.
var distBase = [numDist]uint16{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
var distExtra = [numDist]uint8{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}

// lengthCode maps match length-3 to its length symbol index (0-28).
var lengthCode [maxMatch - minMatch + 1]uint8

// distCodeSmall maps distance-1 below 256 to its symbol; larger distances
// use distCodeLarge indexed by (distance-1)>>7.
var distCodeSmall [256]uint8
var distCodeLarge [256]uint8

// fixedLitLen and fixedDist are the fixed Huffman codes of section 3.2.6.
var fixedLitLen, fixedDist huffmanCode

func init() {
	for code := range lengthBase {
		lo := int(lengthBase[code]) - minMatch
		hi := lo + 1<<lengthExtra[code]
		if code == len(lengthBase)-1 {
			lo, hi = maxMatch-minMatch, maxMatch-minMatch+1
		}
		for l := lo; l < hi && l < len(lengthCode); l++ {
			lengthCode[l] = uint8(code)
		}
	}
	for code := range distBase {
		lo := int(distBase[code]) - 1
		hi := lo + 1<<distExtra[code]
		for d := lo; d < hi; d++ {
			if d < 256 {
				distCodeSmall[d] = uint8(code)
			} else {
				distCodeLarge[d>>7] = uint8(code)
			}
		}
	}
	var lengths [288]uint8
	for i := range lengths {
		switch {
		case i < 144:
			lengths[i] = 8
		case i < 256:
			lengths[i] = 9
		case i < 280:
			lengths[i] = 7
		default:
			lengths[i] = 8
		}
	}
	fixedLitLen.assign(lengths[:])
	var dist [32]uint8
	for i := range dist {
		dist[i] = 5
	}
	fixedDist.assign(dist[:])
}

func distCode(dist int) uint8 {
	if d := dist - 1; d < 256 {
		return distCodeSmall[d]
	} else {
		return distCodeLarge[d>>7]
	}
}

// huffmanCode holds code lengths and bit-reversed canonical codes, ready to
// be written least-significant bit first (RFC 1951 section 3.1.1).
type huffmanCode struct {
	length []uint8
	code   []uint16
}

// assign stores lengths and derives canonical codes (RFC 1951 section 3.2.2).
func (h *huffmanCode) assign(lengths []uint8) {
	h.length = append(h.length[:0], lengths...)
	h.code = slices.Grow(h.code[:0], len(lengths))[:len(lengths)]
	var count [maxCodeBits + 1]uint16
	for _, l := range lengths {
		count[l]++
	}
	count[0] = 0
	var next [maxCodeBits + 2]uint16
	code := uint16(0)
	for b := 1; b <= maxCodeBits; b++ {
		code = (code + count[b-1]) << 1
		next[b] = code
	}
	for i, l := range lengths {
		if l == 0 {
			h.code[i] = 0
			continue
		}
		h.code[i] = bits.Reverse16(next[l]) >> (16 - l)
		next[l]++
	}
}

// bitLength returns the number of bits needed to code freq with h.
func (h *huffmanCode) bitLength(freq []uint32) int {
	n := 0
	for i, f := range freq {
		n += int(f) * int(h.length[i])
	}
	return n
}

// huffmanBuilder computes length-limited code lengths. Its scratch slices are
// reused between blocks so steady-state encoding does not allocate.
type huffmanBuilder struct {
	nodes  []huffmanNode
	leaves []int32
	queue  []int32
	scaled []uint32
}

type huffmanNode struct {
	freq        uint32
	left, right int32 // Child node indices; -1 for leaves.
	symbol      int32
}

// build writes code lengths for freq into lengths, each at most maxBits. At
// least two symbols always receive a code, so the result is a complete prefix
// code that every inflater accepts, even for a block with a single symbol.
func (b *huffmanBuilder) build(freq []uint32, lengths []uint8, maxBits int) {
	clear(lengths)
	b.scaled = append(b.scaled[:0], freq...)
	used := 0
	for _, f := range b.scaled {
		if f != 0 {
			used++
		}
	}
	for i := 0; used < 2; i++ {
		if b.scaled[i] == 0 {
			b.scaled[i] = 1
			used++
		}
	}
	for {
		if b.tryBuild(b.scaled, lengths) <= maxBits {
			return
		}
		// Flatten the distribution and retry. Halving converges: equal weights
		// produce a balanced tree no deeper than ceil(log2(len(freq))).
		for i, f := range b.scaled {
			if f != 0 {
				b.scaled[i] = f>>1 | 1
			}
		}
	}
}

// tryBuild runs the two-queue Huffman construction and returns the depth.
func (b *huffmanBuilder) tryBuild(freq []uint32, lengths []uint8) int {
	b.nodes = b.nodes[:0]
	b.leaves = b.leaves[:0]
	for sym, f := range freq {
		if f != 0 {
			b.nodes = append(b.nodes, huffmanNode{freq: f, left: -1, right: -1, symbol: int32(sym)})
			b.leaves = append(b.leaves, int32(len(b.nodes)-1))
		}
	}
	slices.SortFunc(b.leaves, func(x, y int32) int {
		nx, ny := b.nodes[x], b.nodes[y]
		return cmp.Or(cmp.Compare(nx.freq, ny.freq), cmp.Compare(nx.symbol, ny.symbol))
	})
	// Leaves are consumed in sorted order and internal nodes are created in
	// non-decreasing weight order, so two FIFO queues yield an optimal tree.
	b.queue = b.queue[:0]
	li, qi := 0, 0
	pop := func() int32 {
		if qi < len(b.queue) && (li == len(b.leaves) || b.nodes[b.queue[qi]].freq < b.nodes[b.leaves[li]].freq) {
			qi++
			return b.queue[qi-1]
		}
		li++
		return b.leaves[li-1]
	}
	for len(b.leaves)-li+len(b.queue)-qi > 1 {
		x, y := pop(), pop()
		b.nodes = append(b.nodes, huffmanNode{freq: b.nodes[x].freq + b.nodes[y].freq, left: x, right: y, symbol: -1})
		b.queue = append(b.queue, int32(len(b.nodes)-1))
	}
	// Assign depths iteratively from the root, reusing queue as a stack.
	root := int32(len(b.nodes) - 1)
	maxDepth := 0
	b.queue = append(b.queue[:0], root, 0)
	for len(b.queue) > 0 {
		n, depth := b.queue[len(b.queue)-2], int(b.queue[len(b.queue)-1])
		b.queue = b.queue[:len(b.queue)-2]
		node := b.nodes[n]
		if node.left < 0 {
			lengths[node.symbol] = uint8(min(depth, 255))
			maxDepth = max(maxDepth, depth)
			continue
		}
		b.queue = append(b.queue, node.left, int32(depth+1), node.right, int32(depth+1))
	}
	return maxDepth
}
