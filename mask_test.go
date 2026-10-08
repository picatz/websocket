package websocket

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestMaskMatchesScalar(t *testing.T) {
	sizes := []int{1023, 1024, 1025, 2047, 2048, 2049, 4095, 4096, 4097, 65535, 65536, 65537}
	for n := 0; n <= 257; n++ {
		sizes = append(sizes, n)
	}
	for _, size := range sizes {
		for offset := 0; offset < 16; offset++ {
			for _, value := range []uint32{0, 1, 0x01020304, 0x80ff7f00, 0xffffffff} {
				var key [4]byte
				binary.LittleEndian.PutUint32(key[:], value)
				original := benchmarkPayload(size)
				want := bytes.Clone(original)
				maskScalarReference(key[:], want)
				for _, impl := range []struct {
					name string
					mask func([]byte, []byte)
				}{
					{"production", xor}, {"word", maskWordCandidate}, {"stdlib", maskStandardLibraryCandidate},
				} {
					storage := bytes.Repeat([]byte{0xa5}, size+offset+16)
					data := storage[offset : offset+size]
					copy(data, original)
					impl.mask(key[:], data)
					if !bytes.Equal(data, want) {
						t.Fatalf("%s size=%d offset=%d key=%x: incorrect mask", impl.name, size, offset, key)
					}
					if !bytes.Equal(storage[:offset], bytes.Repeat([]byte{0xa5}, offset)) || !bytes.Equal(storage[offset+size:], bytes.Repeat([]byte{0xa5}, 16)) {
						t.Fatalf("%s size=%d offset=%d: first mask changed guard bytes", impl.name, size, offset)
					}
					if binary.LittleEndian.Uint32(key[:]) != value {
						t.Fatal("first mask mutated key")
					}

					impl.mask(key[:], data)
					if !bytes.Equal(data, original) {
						t.Fatalf("%s size=%d offset=%d: masking twice did not restore data", impl.name, size, offset)
					}
					if !bytes.Equal(storage[:offset], bytes.Repeat([]byte{0xa5}, offset)) || !bytes.Equal(storage[offset+size:], bytes.Repeat([]byte{0xa5}, 16)) {
						t.Fatalf("%s size=%d offset=%d: guard bytes changed", impl.name, size, offset)
					}
					if binary.LittleEndian.Uint32(key[:]) != value {
						t.Fatal("mask key mutated")
					}
				}
			}
		}
	}
}

func FuzzMask(f *testing.F) {
	for _, size := range []int{0, 1, 7, 8, 9, 15, 16, 17, 125, 126, 1023, 1024, 1025, 4095, 4096, 4097} {
		f.Add(benchmarkPayload(size), uint32(0x12345678), uint8(size&15))
	}
	f.Fuzz(func(t *testing.T, original []byte, value uint32, alignment uint8) {
		var key [4]byte
		binary.LittleEndian.PutUint32(key[:], value)
		want := bytes.Clone(original)
		maskScalarReference(key[:], want)
		offset := int(alignment & 15)
		for _, impl := range []struct {
			name string
			mask func([]byte, []byte)
		}{{"production", xor}, {"word", maskWordCandidate}, {"stdlib", maskStandardLibraryCandidate}} {
			storage := bytes.Repeat([]byte{0xa5}, len(original)+offset+16)
			data := storage[offset : offset+len(original)]
			copy(data, original)
			impl.mask(key[:], data)
			if !bytes.Equal(data, want) {
				t.Fatalf("%s: mask differs from scalar reference", impl.name)
			}
			if !bytes.Equal(storage[:offset], bytes.Repeat([]byte{0xa5}, offset)) || !bytes.Equal(storage[offset+len(data):], bytes.Repeat([]byte{0xa5}, 16)) {
				t.Fatalf("%s: first mask changed guard bytes", impl.name)
			}
			if binary.LittleEndian.Uint32(key[:]) != value {
				t.Fatalf("%s: first mask mutated key", impl.name)
			}

			impl.mask(key[:], data)
			if !bytes.Equal(data, original) {
				t.Fatalf("%s: masking twice did not restore data", impl.name)
			}
			if !bytes.Equal(storage[:offset], bytes.Repeat([]byte{0xa5}, offset)) || !bytes.Equal(storage[offset+len(data):], bytes.Repeat([]byte{0xa5}, 16)) {
				t.Fatalf("%s: guard bytes changed", impl.name)
			}
			if binary.LittleEndian.Uint32(key[:]) != value {
				t.Fatalf("%s: mask key mutated", impl.name)
			}
		}
	})
}

// RFC 6455 section 5.7's masked single-frame "Hello" example.
func TestMaskRFC6455Vector(t *testing.T) {
	key := []byte{0x37, 0xfa, 0x21, 0x3d}
	for _, impl := range []struct {
		name string
		mask func([]byte, []byte)
	}{{"production", xor}, {"word", maskWordCandidate}, {"stdlib", maskStandardLibraryCandidate}} {
		data := []byte("Hello")
		impl.mask(key, data)
		if !bytes.Equal(data, []byte{0x7f, 0x9f, 0x4d, 0x51, 0x58}) {
			t.Fatalf("%s: got %x", impl.name, data)
		}
	}
}
