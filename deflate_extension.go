package websocket

import (
	"compress/flate"
	"sync"
)

// perMessageDeflate implements the permessage-deflate extension (RFC 7692).
// Configuration, the actual client offer, and the agreed response are distinct.
type perMessageDeflate struct {
	enabled bool
	server  bool

	configured pmdParameters
	offered    pmdParameters
	offerSet   bool
	negotiated pmdParameters

	level       int // Compression level, as in compress/flate.
	threshold   int // Messages shorter than this are sent uncompressed.
	memoryLimit int // Bytes of state kept between messages; 0 is unlimited.

	receiveMu     sync.Mutex // Receive serialization; never acquired by Close or send.
	receiveLife   sync.Mutex // Short state lifetime guard; never held during I/O.
	receive       *deflateReceiveState
	receiveClosed bool

	send     deflateSender // Used with the connection's write lock held.
	sendInit bool
}

func (pmd *perMessageDeflate) IsEnabled() bool {
	return pmd.enabled
}

// ProcessOutgoingFrame compresses one complete Text or Binary message. Conn
// does not call it: it streams messages through the same compressor, across
// as many frames as needed.
func (pmd *perMessageDeflate) ProcessOutgoingFrame(frame *wireFrame) error {
	if !pmd.enabled {
		return nil
	}
	if frame.Opcode == opContinuation || ((frame.Opcode == opText || frame.Opcode == opBinary) && !frame.Final) {
		return errFragmentedCompression
	}
	if frame.Opcode != opText && frame.Opcode != opBinary {
		return nil
	}
	s := pmd.sender()
	s.begin()
	frame.Payload = s.end(s.write(nil, frame.Payload))
	frame.Rsv1 = true
	return nil
}

func (pmd *perMessageDeflate) ProcessIncomingFrame(frame *wireFrame) error {
	return pmd.processIncomingFrame(frame, 0)
}

// sender configures the send state from the negotiated parameters on first
// use. Negotiation always completes before I/O begins.
func (pmd *perMessageDeflate) sender() *deflateSender {
	if !pmd.sendInit {
		pmd.sendInit = true
		pmd.send = deflateSender{bits: pmd.localWindowBits(), level: pmd.level, retention: retainCompressor}
		if pmd.memoryLimit > 0 {
			// A sender may always use a smaller window than agreed and may
			// always forgo context takeover (RFC 7692 section 7.2.1).
			if bits, ok := pmd.budgetWindowBits(); ok {
				pmd.send.bits = min(pmd.send.bits, bits)
				pmd.send.retention = retainHistory
			} else {
				pmd.send.retention = retainNothing
			}
		}
		if pmd.localNoContextTakeover() {
			pmd.send.retention = retainNothing
		}
	}
	return &pmd.send
}

// pmdOption configures newPerMessageDeflate. Compression is the public form.
type pmdOption func(*perMessageDeflate)

// withClientNoContextTakeover sets the client_no_context_takeover option.
func withClientNoContextTakeover() pmdOption {
	return func(pmd *perMessageDeflate) {
		pmd.configured.clientNoContextTakeover = true
	}
}

// withServerNoContextTakeover sets the server_no_context_takeover option.
func withServerNoContextTakeover() pmdOption {
	return func(pmd *perMessageDeflate) {
		pmd.configured.serverNoContextTakeover = true
	}
}

// withClientMaxWindowBits sets the client_max_window_bits option: the base-2
// logarithm of the LZ77 window a client compresses with, from 8 to 15.
//
// For Dial it is offered as the largest window the client will use; a server
// may select a smaller one. For Upgrade it limits the client's window when the
// client's offer permits that, and such offers are declined otherwise. Values
// outside 8-15 are ignored.
//
// https://www.rfc-editor.org/rfc/rfc7692#section-7.1.2.2
func withClientMaxWindowBits(bits int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if bits >= 8 && bits <= 15 {
			pmd.configured.clientMaxWindowBits = bits
		}
	}
}

// withServerMaxWindowBits sets the server_max_window_bits option: the base-2
// logarithm of the LZ77 window a server compresses with, from 8 to 15.
//
// For Dial it is a constraint the server must accept. For Upgrade the server
// selects this window, or a smaller one the client requests. Smaller windows
// use less memory per connection and compress less. Values outside 8-15 are
// ignored.
//
// https://www.rfc-editor.org/rfc/rfc7692#section-7.1.2.1
func withServerMaxWindowBits(bits int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if bits >= 8 && bits <= 15 {
			pmd.configured.serverMaxWindowBits = bits
		}
	}
}

// withCompressionLevel sets the DEFLATE level used to compress outgoing
// messages, from compress/flate.HuffmanOnly (-2) to
// compress/flate.BestCompression (9). compress/flate.DefaultCompression (-1) is
// the default. Other values are ignored. The level is independent of the window
// size.
func withCompressionLevel(level int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if level >= flate.HuffmanOnly && level <= flate.BestCompression {
			pmd.level = level
		}
	}
}

// withCompressionThreshold sends messages shorter than n bytes uncompressed.
// Compressing a tiny message costs CPU and can make it larger; RFC 7692
// section 6 lets a sender choose per message. Uncompressed messages do not
// enter the compression context. The default, 0, compresses every message.
// Negative values are ignored.
//
// https://www.rfc-editor.org/rfc/rfc7692#section-6
func withCompressionThreshold(n int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if n >= 0 {
			pmd.threshold = n
		}
	}
}

// withCompressionMemoryLimit bounds the compression state one connection keeps
// between messages to n bytes: half for the history it receives with, half for
// the history it sends with. Buffers used while a message is being encoded or
// decoded come from pools shared by all connections and are not counted.
//
// The limit is enforced through negotiation (RFC 7692 section 7.1). Each half
// selects the largest window of 2^8 to 2^15 bytes that fits: a client offers
// server_max_window_bits and a client_max_window_bits hint, and a server
// responds with server_max_window_bits and, when the client offered it,
// client_max_window_bits. Where a window cannot fit, or the peer's window
// cannot be bounded, the corresponding no_context_takeover parameter is used
// instead. A sender with a limit keeps only its window of history and
// re-indexes it for each message, trading CPU for memory; without a limit it
// keeps a whole compressor, which is faster but uses 0.5 to 1 MiB per
// connection.
//
// The default, [DefaultCompressionMemoryLimit], keeps full 2^15-byte windows,
// so it changes no negotiated parameter, only what the sender retains. Pass 0
// to remove the limit. Negative values are ignored.
func withCompressionMemoryLimit(n int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if n >= 0 {
			pmd.memoryLimit = n
		}
	}
}

// DefaultCompressionMemoryLimit is the default [Compression.MemoryLimit]:
// about 64 KiB of state per idle connection, the two 32 KiB histories that
// context takeover with the largest window needs (RFC 7692 section 7.1.1).
const DefaultCompressionMemoryLimit = 64 << 10

// newPerMessageDeflate creates an opt-in permessage-deflate (RFC 7692)
// compression extension. Conn compresses and decompresses whole messages,
// fragmented or not, with context takeover unless a no_context_takeover
// parameter is negotiated, and with any negotiated window from 8 to 15 bits.
//
// Conn applies other enabled extensions to each frame after compression on
// send and before decompression on receive. They may use RSV2 and RSV3, but
// RSV1 belongs to permessage-deflate (RFC 7692 section 5); a frame whose RSV1
// another extension changed fails with errUnsupportedExtensionComposition,
// as does enabling more than one permessage-deflate instance.
//
// Each connection needs its own instance; Compression creates one per
// connection. Direct callbacks require complete messages; do not mix them
// concurrently with Conn I/O. Configuration and negotiation must finish before
// I/O; concurrent reconfiguration is not supported.
//
// https://www.rfc-editor.org/rfc/rfc7692
func newPerMessageDeflate(options ...pmdOption) frameExtension {
	pmd := &perMessageDeflate{level: flate.DefaultCompression, memoryLimit: DefaultCompressionMemoryLimit}
	for _, option := range options {
		option(pmd)
	}
	return pmd
}
