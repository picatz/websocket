package websocket

import (
	"encoding/binary"
)

// xor XORs the payload with the masking key,
// updating the payload in place to avoid additional
// allocations.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.3
func xor(key []byte, data []byte) {
	// A whole number of four-byte key cycles preserves the mask phase, so the
	// key repeats across a word. Using encoding/binary keeps the word
	// operations safe for unaligned slices and independent of byte order.
	k := uint64(binary.LittleEndian.Uint32(key))
	k |= k << 32
	// Four independent words per iteration let the CPU overlap the loads and
	// stores, more than doubling throughput from 1 KiB up.
	for len(data) >= 32 {
		d := (*[32]byte)(data)
		binary.LittleEndian.PutUint64(d[0:], binary.LittleEndian.Uint64(d[0:])^k)
		binary.LittleEndian.PutUint64(d[8:], binary.LittleEndian.Uint64(d[8:])^k)
		binary.LittleEndian.PutUint64(d[16:], binary.LittleEndian.Uint64(d[16:])^k)
		binary.LittleEndian.PutUint64(d[24:], binary.LittleEndian.Uint64(d[24:])^k)
		data = data[32:]
	}
	for len(data) >= 8 {
		binary.LittleEndian.PutUint64(data, binary.LittleEndian.Uint64(data)^k)
		data = data[8:]
	}
	for i := range data {
		data[i] ^= byte(k >> (8 * (i & 3)))
	}
}
