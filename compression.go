package websocket

import (
	"compress/flate"
	"fmt"
)

// Compression configures permessage-deflate (RFC 7692) for a Dialer, Upgrader
// or ConnConfig. Its zero value negotiates context takeover with full 2^15-byte
// windows, compresses every message at flate.DefaultCompression, and bounds
// retained state to DefaultCompressionMemoryLimit per connection. A Compression
// holds settings only, so one value may configure every connection; Dial and
// Upgrade create each connection's compressor state.
//
// https://www.rfc-editor.org/rfc/rfc7692
type Compression struct {
	// Level is the DEFLATE level for outgoing messages, from
	// flate.HuffmanOnly (-2) to flate.BestCompression (9). Zero means
	// flate.DefaultCompression, so flate.NoCompression cannot be selected;
	// use Threshold to send small messages uncompressed, or leave
	// Compression nil. The level is independent of the window.
	Level int

	// Threshold sends messages shorter than this many bytes uncompressed.
	// Compressing a tiny message costs CPU and can make it larger; RFC 7692
	// section 6 lets a sender choose per message. Uncompressed messages do
	// not enter the compression context. Zero compresses every message; a
	// negative value is invalid.
	Threshold int

	// MemoryLimit bounds the compression state one connection keeps between
	// messages: half for the history it receives with, half for the history
	// it sends with. Each half selects, through negotiation, the largest
	// window of 2^8 to 2^15 bytes that fits, or no_context_takeover where
	// none can. Zero means DefaultCompressionMemoryLimit, which keeps full
	// windows; a negative value means no limit, which keeps a whole
	// compressor per connection (0.5 to 1 MiB) in exchange for less CPU.
	//
	// https://www.rfc-editor.org/rfc/rfc7692#section-7.1
	MemoryLimit int

	// ClientNoContextTakeover and ServerNoContextTakeover request that the
	// client or server reset its compression context after every message
	// (client_no_context_takeover and server_no_context_takeover).
	//
	// https://www.rfc-editor.org/rfc/rfc7692#section-7.1.1
	ClientNoContextTakeover bool
	ServerNoContextTakeover bool

	// ClientMaxWindowBits and ServerMaxWindowBits are the base-2 logarithm of
	// the LZ77 window the client or server compresses with, from 8 to 15.
	// Zero leaves the parameter out. A Dialer offers ClientMaxWindowBits as
	// the largest window it will use and requires ServerMaxWindowBits of the
	// server; an Upgrader selects ServerMaxWindowBits, or a smaller window the
	// client requests, and limits the client's window when its offer permits.
	//
	// https://www.rfc-editor.org/rfc/rfc7692#section-7.1.2
	ClientMaxWindowBits int
	ServerMaxWindowBits int
}

// extension validates c and creates one connection's permessage-deflate
// extension from it.
func (c *Compression) extension() (frameExtension, error) {
	switch {
	case c.Level < flate.HuffmanOnly || c.Level > flate.BestCompression:
		return nil, fmt.Errorf("%w: Compression.Level %d is outside -2 to 9", ErrInvalidConfig, c.Level)
	case c.Threshold < 0:
		return nil, fmt.Errorf("%w: Compression.Threshold %d is negative", ErrInvalidConfig, c.Threshold)
	case !validWindowBits(c.ClientMaxWindowBits):
		return nil, fmt.Errorf("%w: Compression.ClientMaxWindowBits %d is outside 8 to 15", ErrInvalidConfig, c.ClientMaxWindowBits)
	case !validWindowBits(c.ServerMaxWindowBits):
		return nil, fmt.Errorf("%w: Compression.ServerMaxWindowBits %d is outside 8 to 15", ErrInvalidConfig, c.ServerMaxWindowBits)
	}
	options := []pmdOption{
		withCompressionThreshold(c.Threshold),
		withClientMaxWindowBits(c.ClientMaxWindowBits),
		withServerMaxWindowBits(c.ServerMaxWindowBits),
	}
	if c.Level != 0 {
		options = append(options, withCompressionLevel(c.Level))
	}
	switch {
	case c.MemoryLimit > 0:
		options = append(options, withCompressionMemoryLimit(c.MemoryLimit))
	case c.MemoryLimit < 0:
		options = append(options, withCompressionMemoryLimit(0))
	}
	if c.ClientNoContextTakeover {
		options = append(options, withClientNoContextTakeover())
	}
	if c.ServerNoContextTakeover {
		options = append(options, withServerNoContextTakeover())
	}
	return newPerMessageDeflate(options...), nil
}

func validWindowBits(bits int) bool { return bits == 0 || bits >= 8 && bits <= 15 }
