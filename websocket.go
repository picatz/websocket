package websocket

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrBadHandshake            = errors.New("websocket: bad handshake")
	ErrUnsupportedVersion      = errors.New("websocket: unsupported WebSocket version")
	ErrInvalidUpgradeHeader    = errors.New("websocket: invalid Upgrade header")
	ErrInvalidConnectionHeader = errors.New("websocket: invalid Connection header")
	ErrMissingSecKey           = errors.New("websocket: missing Sec-WebSocket-Key header")
	ErrInvalidSecAccept        = errors.New("websocket: invalid Sec-WebSocket-Accept header")
	ErrNotHijacker             = errors.New("websocket: response does not implement http.Hijacker")
	ErrInvalidMethod           = errors.New("websocket: invalid request method")
	ErrHandshakeFailed         = errors.New("websocket: handshake failed")
	ErrFailedToGenerateKey     = errors.New("websocket: failed to generate key")
	ErrInvalidFrame            = errors.New("websocket: invalid frame")
	ErrUnexpectedFrame         = errors.New("websocket: unexpected frame")
	ErrControlFrameFragment    = errors.New("websocket: control frames cannot be fragmented")
	ErrPayloadTooLarge         = errors.New("websocket: payload too large")
	ErrUnexpectedContinuation  = errors.New("websocket: unexpected continuation frame")
	ErrUnmaskedFrame           = errors.New("websocket: received unmasked frame from client")
	ErrMaskedFrame             = errors.New("websocket: received masked frame from server")
	ErrUnsupportedExtensions   = errors.New("websocket: unsupported extensions (RSV bits are set)")
	ErrInvalidOpcode           = errors.New("websocket: invalid opcode")
	ErrWriteFailed             = errors.New("websocket: write failed")
	ErrAlreadyClosed           = errors.New("websocket: connection already closed")
)

type Opcode int

const (
	ContinuationFrame Opcode = 0x0 // Continuation frame opcode.
	TextMessage       Opcode = 0x1 // Text message opcode.
	BinaryMessage     Opcode = 0x2 // Binary message opcode.
	CloseMessage      Opcode = 0x8 // Close message opcode.
	PingMessage       Opcode = 0x9 // Ping message opcode.
	PongMessage       Opcode = 0xA // Pong message opcode.
)

func (o Opcode) String() string {
	switch o {
	case ContinuationFrame:
		return "ContinuationFrame"
	case TextMessage:
		return "TextMessage"
	case BinaryMessage:
		return "BinaryMessage"
	case CloseMessage:
		return "CloseMessage"
	case PingMessage:
		return "PingMessage"
	case PongMessage:
		return "PongMessage"
	default:
		return "Unknown"
	}
}

// Frame represents a WebSocket frame.
//
// https://tools.ietf.org/html/rfc6455#section-5.2
type Frame struct {
	Final   bool
	Opcode  Opcode
	Payload []byte
	Masked  bool
	MaskKey [4]byte
	Rsv1    bool
	Rsv2    bool
	Rsv3    bool
}

// Extension interface for WebSocket extensions.
//
// https://tools.ietf.org/html/rfc6455#section-9
type Extension interface {
	// Name returns the extension name.
	Name() string
	// Offer returns the extension offer string for the handshake.
	Offer() string
	// Negotiate negotiates the extension parameters based on the response from the peer.
	Negotiate(response string) error
	// ProcessOutgoingFrame allows the extension to modify outgoing frames.
	ProcessOutgoingFrame(frame *Frame) error
	// ProcessIncomingFrame allows the extension to modify incoming frames.
	ProcessIncomingFrame(frame *Frame) error
	// IsEnabled indicates whether the extension is enabled.
	IsEnabled() bool
}

// perMessageDeflate implements permessage-deflate as an Extension.
type perMessageDeflate struct {
	enabled bool

	// Compression options
	clientNoContextTakeover bool
	serverNoContextTakeover bool

	serverMaxWindowBits int // Allowed values: 8-15
	clientMaxWindowBits int // Allowed values: 8-15

	flateReaderPool sync.Pool
	flateWriterPool sync.Pool
}

func (pmd *perMessageDeflate) Name() string {
	return "permessage-deflate"
}

func (pmd *perMessageDeflate) Offer() string {
	params := []string{"permessage-deflate"}
	if pmd.clientNoContextTakeover {
		params = append(params, "client_no_context_takeover")
	}
	if pmd.serverNoContextTakeover {
		params = append(params, "server_no_context_takeover")
	}
	if pmd.clientMaxWindowBits > 0 {
		params = append(params, fmt.Sprintf("client_max_window_bits=%d", pmd.clientMaxWindowBits))
	}
	if pmd.serverMaxWindowBits > 0 {
		params = append(params, fmt.Sprintf("server_max_window_bits=%d", pmd.serverMaxWindowBits))
	}
	return strings.Join(params, "; ")
}

func (pmd *perMessageDeflate) Negotiate(response string) error {
	if strings.Contains(response, "permessage-deflate") {
		pmd.enabled = true
		// Parse parameters
		params := parseExtensionParams(response)
		if _, ok := params["client_no_context_takeover"]; ok {
			pmd.clientNoContextTakeover = true
		}
		if _, ok := params["server_no_context_takeover"]; ok {
			pmd.serverNoContextTakeover = true
		}
		if val, ok := params["client_max_window_bits"]; ok {
			if val == "" {
				// The server is asking the client to choose the window size
				// We'll use the default of 15
				pmd.clientMaxWindowBits = 15
			} else {
				bits, err := strconv.Atoi(val)
				if err != nil || bits < 8 || bits > 15 {
					return fmt.Errorf("invalid client_max_window_bits: %v", val)
				}
				pmd.clientMaxWindowBits = bits
			}
		}
		if val, ok := params["server_max_window_bits"]; ok {
			bits, err := strconv.Atoi(val)
			if err != nil || bits < 8 || bits > 15 {
				return fmt.Errorf("invalid server_max_window_bits: %v", val)
			}
			pmd.serverMaxWindowBits = bits
		}
	}
	return nil
}

func (pmd *perMessageDeflate) IsEnabled() bool {
	return pmd.enabled
}

func (pmd *perMessageDeflate) ProcessOutgoingFrame(frame *Frame) error {
	if !pmd.enabled || (frame.Opcode != TextMessage && frame.Opcode != BinaryMessage) {
		return nil
	}

	// Compress the payload
	var buf bytes.Buffer
	w := pmd.getWriter(&buf)
	defer pmd.putWriter(w)

	if _, err := w.Write(frame.Payload); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	compressed := buf.Bytes()
	// Remove the last four bytes (empty DEFLATE block) if present
	if len(compressed) >= 4 {
		compressed = compressed[:len(compressed)-4]
	}
	frame.Payload = compressed

	// Set RSV1 bit to indicate compression
	frame.Rsv1 = true

	return nil
}

func (pmd *perMessageDeflate) ProcessIncomingFrame(frame *Frame) error {
	if !pmd.enabled || (frame.Opcode != TextMessage && frame.Opcode != BinaryMessage) {
		return nil
	}

	if frame.Rsv1 {
		// Decompress the payload
		// Append empty DEFLATE block
		payload := append(frame.Payload, 0x00, 0x00, 0xff, 0xff)
		r := pmd.getReader(bytes.NewReader(payload))
		defer pmd.putReader(r)

		decompressed, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if err := r.Close(); err != nil {
			return err
		}
		frame.Payload = decompressed
		frame.Rsv1 = false
	}
	return nil
}

// getWriter retrieves or creates a flate.Writer, resetting it if necessary.
func (pmd *perMessageDeflate) getWriter(buf *bytes.Buffer) *flate.Writer {
	var w *flate.Writer
	if pmd.serverNoContextTakeover {
		w, _ = flate.NewWriter(buf, pmd.serverMaxWindowBitsToLevel())
	} else {
		if v := pmd.flateWriterPool.Get(); v != nil {
			w = v.(*flate.Writer)
			w.Reset(buf)
		} else {
			w, _ = flate.NewWriter(buf, pmd.serverMaxWindowBitsToLevel())
		}
	}
	return w
}

// putWriter returns the flate.Writer to the pool if context takeover is enabled.
func (pmd *perMessageDeflate) putWriter(w *flate.Writer) {
	if !pmd.serverNoContextTakeover {
		pmd.flateWriterPool.Put(w)
	}
}

// getReader retrieves or creates a flate.Reader, resetting it if necessary.
func (pmd *perMessageDeflate) getReader(r io.Reader) io.ReadCloser {
	var fr io.ReadCloser
	if pmd.clientNoContextTakeover {
		fr = flate.NewReader(r)
	} else {
		if v := pmd.flateReaderPool.Get(); v != nil {
			fr = v.(io.ReadCloser)
			resetter := fr.(flate.Resetter)
			resetter.Reset(r, nil)
		} else {
			fr = flate.NewReader(r)
		}
	}
	return fr
}

// putReader returns the flate.Reader to the pool if context takeover is enabled.
func (pmd *perMessageDeflate) putReader(r io.ReadCloser) {
	if !pmd.clientNoContextTakeover {
		pmd.flateReaderPool.Put(r)
	}
}

// serverMaxWindowBitsToLevel converts the serverMaxWindowBits to a compression level.
func (pmd *perMessageDeflate) serverMaxWindowBitsToLevel() int {
	// Go's flate package uses levels from 1 (BestSpeed) to 9 (BestCompression)
	// Map window bits 8-15 to levels 1-9
	if pmd.serverMaxWindowBits >= 8 && pmd.serverMaxWindowBits <= 15 {
		return (pmd.serverMaxWindowBits - 7) // windowBits 8 maps to level 1
	}
	return flate.DefaultCompression
}

// parseExtensionParams parses the extension parameters from the header value.
func parseExtensionParams(s string) map[string]string {
	params := make(map[string]string)
	parts := strings.Split(s, ";")
	for _, part := range parts[1:] { // Skip the extension name
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "=") {
			kv := strings.SplitN(part, "=", 2)
			key := strings.TrimSpace(kv[0])
			value := strings.TrimSpace(kv[1])
			params[key] = value
		} else {
			params[part] = ""
		}
	}
	return params
}

// PerMessageDeflateOption represents an option for permessage-deflate extension.
type PerMessageDeflateOption func(*perMessageDeflate)

// WithClientNoContextTakeover sets the client_no_context_takeover option.
func WithClientNoContextTakeover() PerMessageDeflateOption {
	return func(pmd *perMessageDeflate) {
		pmd.clientNoContextTakeover = true
	}
}

// WithServerNoContextTakeover sets the server_no_context_takeover option.
func WithServerNoContextTakeover() PerMessageDeflateOption {
	return func(pmd *perMessageDeflate) {
		pmd.serverNoContextTakeover = true
	}
}

// WithClientMaxWindowBits sets the client_max_window_bits option.
func WithClientMaxWindowBits(bits int) PerMessageDeflateOption {
	return func(pmd *perMessageDeflate) {
		if bits >= 8 && bits <= 15 {
			pmd.clientMaxWindowBits = bits
		}
	}
}

// WithServerMaxWindowBits sets the server_max_window_bits option.
func WithServerMaxWindowBits(bits int) PerMessageDeflateOption {
	return func(pmd *perMessageDeflate) {
		if bits >= 8 && bits <= 15 {
			pmd.serverMaxWindowBits = bits
		}
	}
}

// NewPerMessageDeflateExtension creates a new permessage-deflate extension with optional settings.
func NewPerMessageDeflateExtension(options ...PerMessageDeflateOption) Extension {
	pmd := &perMessageDeflate{
		flateReaderPool: sync.Pool{
			New: func() any {
				return flate.NewReaderDict(nil, nil)
			},
		},
		flateWriterPool: sync.Pool{
			New: func() any {
				w, _ := flate.NewWriter(nil, flate.DefaultCompression)
				return w
			},
		},
	}
	for _, option := range options {
		option(pmd)
	}
	return pmd
}

// Conn represents a WebSocket connection.
type Conn struct {
	conn     net.Conn          // Underlying network connection
	rw       *bufio.ReadWriter // Buffered reader and writer
	isServer bool              // True if this Conn is on the server side
	maxBytes int               // Maximum message size in bytes

	readMu  sync.Mutex // Protects read operations
	writeMu sync.Mutex // Protects write operations
	closeMu sync.Mutex // Protects Close method

	closed   bool  // Indicates if the connection is closed
	closeErr error // Stores the error from closing the connection

	// Optional handlers for control frames
	pingHandler func(data string) error // Handler for Ping frames
	pongHandler func(data string) error // Handler for Pong frames

	// Extensions
	extensions []Extension
}

type ConnOption func(*Conn)

func WithMaxBytes(maxBytes int) ConnOption {
	return func(c *Conn) {
		if maxBytes > 0 {
			c.maxBytes = maxBytes
		}
	}
}

// NewConn creates a new WebSocket connection.
func NewConn(conn net.Conn, isServer bool, extensions []Extension, opts ...ConnOption) *Conn {
	wsConn := &Conn{
		conn:       conn,
		rw:         bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)),
		isServer:   isServer,
		extensions: extensions,
	}

	for _, opt := range opts {
		opt(wsConn)
	}

	return wsConn
}

// Endpoints MAY use the following pre-defined status codes
// when sending a Close frame.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.1
const (
	StatusNormalClosure           = 1000
	StatusGoingAway               = 1001
	StatusProtocolError           = 1002
	StatusUnsupportedData         = 1003
	StatusNoStatusReceived        = 1005
	StatusAbnormalClosure         = 1006
	StatusInvalidFramePayloadData = 1007
	StatusPolicyViolation         = 1008
	StatusMessageTooBig           = 1009
	StatusMandatoryExtension      = 1010
	StatusInternalServerError     = 1011
	StatusTLSHandshake            = 1015
)

// Close gracefully closes the WebSocket connection.
func (c *Conn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return ErrAlreadyClosed
	}
	c.closed = true

	// Send a close frame to the peer (best effort)
	closePayload := make([]byte, 2)
	binary.BigEndian.PutUint16(closePayload, StatusNormalClosure)
	_ = c.WriteControlFrame(CloseMessage, closePayload)

	// Close the underlying connection
	c.closeErr = c.conn.Close()
	return c.closeErr
}

// SetPingHandler sets the handler function for Ping frames.
func (c *Conn) SetPingHandler(handler func(appData string) error) {
	c.pingHandler = handler
}

// SetPongHandler sets the handler function for Pong frames.
func (c *Conn) SetPongHandler(handler func(appData string) error) {
	c.pongHandler = handler
}

// ReadMessage reads the next complete WebSocket message.
func (c *Conn) ReadMessage() (messageType Opcode, data []byte, err error) {
	if c.closed {
		return 0, nil, io.ErrClosedPipe
	}

	c.readMu.Lock()
	defer c.readMu.Unlock()

	var message []byte
	var messageTypeSet bool
	for {
		frame, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch frame.Opcode {
		case TextMessage, BinaryMessage:
			if !messageTypeSet {
				messageType = frame.Opcode
				messageTypeSet = true
			}

			if c.maxBytes > 0 && len(message)+len(frame.Payload) > c.maxBytes {
				return 0, nil, fmt.Errorf("%w: message too large: %d", ErrPayloadTooLarge, len(message)+len(frame.Payload))
			}

			message = append(message, frame.Payload...)
			if frame.Final {
				return messageType, message, nil
			}

		case ContinuationFrame:
			message = append(message, frame.Payload...)
			if frame.Final {
				return messageType, message, nil
			}

		case CloseMessage:
			// Respond with a close frame if not already sent
			closePayload := make([]byte, 2)
			binary.BigEndian.PutUint16(closePayload, StatusNormalClosure)
			_ = c.WriteControlFrame(CloseMessage, closePayload)
			return 0, nil, io.EOF

		case PingMessage:
			if c.pingHandler != nil {
				if err := c.pingHandler(string(frame.Payload)); err != nil {
					return 0, nil, err
				}
			} else {
				// Default behavior: send Pong frame with same payload
				if err := c.WriteControlFrame(PongMessage, frame.Payload); err != nil {
					return 0, nil, err
				}
			}

		case PongMessage:
			if c.pongHandler != nil {
				if err := c.pongHandler(string(frame.Payload)); err != nil {
					return 0, nil, err
				}
			}
			// Default behavior: ignore Pong frames

		default:
			return 0, nil, fmt.Errorf("%w: %d", ErrInvalidOpcode, frame.Opcode)
		}
	}
}

// WriteMessage writes a WebSocket message.
func (c *Conn) WriteMessage(messageType Opcode, data []byte) error {
	if c.closed {
		return io.ErrClosedPipe
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	frame := &Frame{
		Final:   true,
		Opcode:  messageType,
		Payload: data,
		Masked:  !c.isServer,
	}
	return c.writeFrame(frame)
}

// WriteControlFrame writes a control frame to the connection.
func (c *Conn) WriteControlFrame(opcode Opcode, data []byte) error {
	if len(data) > 125 {
		return fmt.Errorf("%w: control frame payload too large: %d", ErrPayloadTooLarge, len(data))
	}
	if c.closed {
		return io.ErrClosedPipe
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	frame := &Frame{
		Final:   true,
		Opcode:  opcode,
		Payload: data,
		Masked:  !c.isServer,
	}
	return c.writeFrame(frame)
}

// readFrame reads a single WebSocket frame from the connection.
func (c *Conn) readFrame() (*Frame, error) {
	// Read the first two bytes of the frame header, which
	// contains the FIN, RSV1, RSV2, RSV3, opcode, and mask bit.
	var header [2]byte
	if _, err := io.ReadFull(c.rw, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("failed to read frame header: %w", err)
	}

	b0 := header[0]
	b1 := header[1]

	frame := &Frame{
		Final:  (b0 & 0x80) != 0,
		Opcode: Opcode(b0 & 0x0F),
		Masked: (b1 & 0x80) != 0,
		Rsv1:   (b0 & 0x40) != 0,
		Rsv2:   (b0 & 0x20) != 0,
		Rsv3:   (b0 & 0x10) != 0,
	}
	payloadLen := int(b1 & 0x7F)

	// Control frames must not be fragmented
	if !frame.Final && frame.Opcode >= 0x8 {
		return nil, ErrControlFrameFragment
	}

	// Control frames must have payload length <= 125
	if frame.Opcode >= 0x8 && payloadLen > 125 {
		return nil, fmt.Errorf("%w: control frame payload too large: %d", ErrPayloadTooLarge, payloadLen)
	}

	// Read extended payload length if necessary
	switch payloadLen {
	case 126:
		var extLen uint16
		if err := binary.Read(c.rw, binary.BigEndian, &extLen); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("failed to read extended payload length: %w", err)
		}
		payloadLen = int(extLen)
	case 127:
		var extLen uint64
		if err := binary.Read(c.rw, binary.BigEndian, &extLen); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("failed to read extended payload length: %w", err)
		}
		if extLen > (1 << 63) {
			return nil, ErrPayloadTooLarge
		}
		payloadLen = int(extLen)
	}

	// Read masking key if necessary
	if frame.Masked {
		if _, err := io.ReadFull(c.rw, frame.MaskKey[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("failed to read masking key: %w", err)
		}
	} else if c.isServer {
		// Servers must receive masked frames from clients
		return nil, ErrUnmaskedFrame
	}

	// Read payload data
	if payloadLen > 0 {
		frame.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(c.rw, frame.Payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("failed to read payload data: %w", err)
		}

		// Unmask payload if necessary
		if frame.Masked {
			xor(frame.MaskKey[:], frame.Payload)
		}
	}

	// Process incoming frame through extensions
	for _, ext := range c.extensions {
		if ext.IsEnabled() {
			if err := ext.ProcessIncomingFrame(frame); err != nil {
				return nil, fmt.Errorf("extension %s failed to process incoming frame: %w", ext.Name(), err)
			}
		}
	}

	return frame, nil
}

// writeFrame writes a WebSocket frame to the connection.
func (c *Conn) writeFrame(frame *Frame) error {
	// Process outgoing frame through extensions
	for _, ext := range c.extensions {
		if ext.IsEnabled() {
			if err := ext.ProcessOutgoingFrame(frame); err != nil {
				return fmt.Errorf("extension %s failed to process outgoing frame: %v", ext.Name(), err)
			}
		}
	}

	b0 := byte(frame.Opcode)
	if frame.Final {
		b0 |= 0x80
	}
	if frame.Rsv1 {
		b0 |= 0x40
	}
	if frame.Rsv2 {
		b0 |= 0x20
	}
	if frame.Rsv3 {
		b0 |= 0x10
	}

	b1 := byte(0)
	if frame.Masked {
		b1 |= 0x80
	}

	payloadLen := len(frame.Payload)
	var header [14]byte // Maximum header size

	headerPos := 0
	header[headerPos] = b0
	headerPos++
	header[headerPos] = b1
	headerPos++

	// Extended payload length
	if payloadLen <= 125 {
		header[1] |= byte(payloadLen)
	} else if payloadLen <= 65535 {
		header[1] |= 126
		binary.BigEndian.PutUint16(header[headerPos:], uint16(payloadLen))
		headerPos += 2
	} else {
		header[1] |= 127
		binary.BigEndian.PutUint64(header[headerPos:], uint64(payloadLen))
		headerPos += 8
	}

	// Masking key
	if frame.Masked {
		if _, err := rand.Read(frame.MaskKey[:]); err != nil {
			return fmt.Errorf("%w: failed to generate masking key: %v", ErrWriteFailed, err)
		}
		copy(header[headerPos:], frame.MaskKey[:])
		headerPos += 4
	}

	// Write header
	if _, err := c.rw.Write(header[:headerPos]); err != nil {
		return fmt.Errorf("%w: failed to write frame header: %v", ErrWriteFailed, err)
	}

	// Mask payload if necessary
	if frame.Masked && len(frame.Payload) > 0 {
		xor(frame.MaskKey[:], frame.Payload)
		defer xor(frame.MaskKey[:], frame.Payload) // Unmask after sending
	}

	// Write payload
	if len(frame.Payload) > 0 {
		if _, err := c.rw.Write(frame.Payload); err != nil {
			return fmt.Errorf("%w: failed to write frame payload: %v", ErrWriteFailed, err)
		}
	}

	// Flush the buffer to ensure the data is sent
	if err := c.rw.Flush(); err != nil {
		return fmt.Errorf("%w: failed to flush data: %v", ErrWriteFailed, err)
	}

	return nil
}

// DialOption represents an option for the Dial function.
type DialOption func(*dialOptions)

// dialOptions stores the options for the Dial function.
type dialOptions struct {
	extensions []Extension
	header     http.Header
	tlsConfig  *tls.Config
	maxBytes   int
}

// apply applies the options to the dialOptions.
func (opts *dialOptions) apply(options []DialOption) {
	for _, option := range options {
		if option != nil {
			option(opts)
		}
	}
}

// WithExtensions sets the extensions to use during the WebSocket handshake.
func WithExtensions(extensions ...Extension) DialOption {
	return func(opts *dialOptions) {
		opts.extensions = extensions
	}
}

// WithHeader sets custom headers for the WebSocket handshake.
func WithHeader(header http.Header) DialOption {
	return func(opts *dialOptions) {
		opts.header = header
	}
}

// WithTLSConfig sets the TLS configuration for the WebSocket connection.
func WithTLSConfig(config *tls.Config) DialOption {
	return func(opts *dialOptions) {
		opts.tlsConfig = config
	}
}

// WithMaxMessageSize sets the maximum message size in bytes.
func WithMaxMessageSize(maxBytes int) DialOption {
	return func(opts *dialOptions) {
		opts.maxBytes = maxBytes
	}
}

// Dial establishes a WebSocket client connection to the given URL.
func Dial(ctx context.Context, urlStr string, options ...DialOption) (*Conn, *http.Response, error) {
	opts := &dialOptions{}
	opts.apply(options)

	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid URL: %v", ErrBadHandshake, err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	// Establish the network connection
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: failed to dial: %v", ErrBadHandshake, err)
	}

	// If using TLS, perform the handshake
	if u.Scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: u.Hostname(),
			NextProtos: []string{"http/1.1"},
		})
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("%w: failed to perform TLS handshake: %v", ErrBadHandshake, err)
		}
		conn = tlsConn
	}

	key, err := generateKey()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: %v", ErrFailedToGenerateKey, err)
	}

	// Build the request
	var reqBuilder strings.Builder
	reqBuilder.Grow(512)
	reqBuilder.WriteString("GET " + u.RequestURI() + " HTTP/1.1\r\n")
	reqBuilder.WriteString("Host: " + u.Host + "\r\n")
	reqBuilder.WriteString("Upgrade: websocket\r\n")
	reqBuilder.WriteString("Connection: Upgrade\r\n")
	reqBuilder.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	reqBuilder.WriteString("Sec-WebSocket-Version: 13\r\n")

	// Handle extensions
	if len(opts.extensions) > 0 {
		var offers []string
		for _, ext := range opts.extensions {
			offers = append(offers, ext.Offer())
		}
		reqBuilder.WriteString("Sec-WebSocket-Extensions: " + strings.Join(offers, ", ") + "\r\n")
	}

	// Custom headers
	if opts.header != nil {
		for k, vs := range opts.header {
			for _, v := range vs {
				reqBuilder.WriteString(k)
				reqBuilder.WriteString(": ")
				reqBuilder.WriteString(v)
				reqBuilder.WriteString("\r\n")
			}
		}
	}
	reqBuilder.WriteString("\r\n")
	reqStr := reqBuilder.String()

	// Send the request
	if _, err = conn.Write([]byte(reqStr)); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: failed to send handshake request: %v", ErrBadHandshake, err)
	}

	// Read the response
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: failed to read handshake response: %v", ErrBadHandshake, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, resp, fmt.Errorf("%w: unexpected status code %d", ErrBadHandshake, resp.StatusCode)
	}
	if !headerContains(resp.Header, "Connection", "Upgrade") {
		conn.Close()
		return nil, resp, ErrInvalidConnectionHeader
	}
	if !headerContains(resp.Header, "Upgrade", "websocket") {
		conn.Close()
		return nil, resp, ErrInvalidUpgradeHeader
	}
	accept := resp.Header.Get("Sec-WebSocket-Accept")
	if accept == "" {
		conn.Close()
		return nil, resp, ErrInvalidSecAccept
	}
	expectedAccept := computeAcceptKey(key)
	if accept != expectedAccept {
		conn.Close()
		return nil, resp, ErrInvalidSecAccept
	}

	// Negotiate extensions
	extHeader := resp.Header.Get("Sec-WebSocket-Extensions")
	for _, ext := range opts.extensions {
		if err := ext.Negotiate(extHeader); err != nil {
			conn.Close()
			return nil, resp, fmt.Errorf("failed to negotiate extension %s: %v", ext.Name(), err)
		}
	}

	return NewConn(conn, false, opts.extensions, WithMaxBytes(opts.maxBytes)), resp, nil
}

// UpgradeOption represents an option for the Upgrade function.
type UpgradeOption func(*upgradeOptions)

// upgradeOptions stores the options for the Upgrade function.
type upgradeOptions struct {
	extensions     []Extension
	responseHeader http.Header
}

// apply applies the options to the upgradeOptions.
func (opts *upgradeOptions) apply(options []UpgradeOption) {
	for _, option := range options {
		if option != nil {
			option(opts)
		}
	}
}

// WithUpgradeExtensions sets the extensions to use during the WebSocket handshake.
func WithUpgradeExtensions(extensions ...Extension) UpgradeOption {
	return func(opts *upgradeOptions) {
		opts.extensions = extensions
	}
}

// WithResponseHeader sets custom headers for the WebSocket handshake response.
func WithResponseHeader(header http.Header) UpgradeOption {
	return func(opts *upgradeOptions) {
		opts.responseHeader = header
	}
}

// Upgrade upgrades the HTTP server connection to a WebSocket connection.
func Upgrade(w http.ResponseWriter, r *http.Request, options ...UpgradeOption) (*Conn, error) {
	opts := &upgradeOptions{}
	opts.apply(options)

	if !headerContains(r.Header, "Connection", "Upgrade") {
		return nil, ErrInvalidConnectionHeader
	}
	if !headerContains(r.Header, "Upgrade", "websocket") {
		return nil, ErrInvalidUpgradeHeader
	}
	if r.Method != http.MethodGet {
		return nil, ErrInvalidMethod
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, ErrUnsupportedVersion
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, ErrMissingSecKey
	}
	acceptKey := computeAcceptKey(key)

	// Handle extensions
	var responseHeader http.Header
	if opts.responseHeader != nil {
		responseHeader = opts.responseHeader.Clone()
	} else {
		responseHeader = make(http.Header)
	}

	var acceptedExtensions []string
	if len(opts.extensions) > 0 {
		extHeader := r.Header.Get("Sec-WebSocket-Extensions")
		for _, ext := range opts.extensions {
			if err := ext.Negotiate(extHeader); err != nil {
				return nil, fmt.Errorf("failed to negotiate extension %s: %v", ext.Name(), err)
			}
			if ext.IsEnabled() {
				acceptedExtensions = append(acceptedExtensions, ext.Offer())
			}
		}
		if len(acceptedExtensions) > 0 {
			responseHeader.Add("Sec-WebSocket-Extensions", strings.Join(acceptedExtensions, ", "))
		}
	}

	// Hijack the connection
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, ErrNotHijacker
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}

	// Build the response
	var responseBuilder strings.Builder
	responseBuilder.Grow(256)
	responseBuilder.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	responseBuilder.WriteString("Upgrade: websocket\r\n")
	responseBuilder.WriteString("Connection: Upgrade\r\n")
	responseBuilder.WriteString("Sec-WebSocket-Accept: ")
	responseBuilder.WriteString(acceptKey)
	responseBuilder.WriteString("\r\n")

	for k, vs := range responseHeader {
		for _, v := range vs {
			responseBuilder.WriteString(k)
			responseBuilder.WriteString(": ")
			responseBuilder.WriteString(v)
			responseBuilder.WriteString("\r\n")
		}
	}
	responseBuilder.WriteString("\r\n")

	// Send the response
	if _, err = bufrw.WriteString(responseBuilder.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: failed to write handshake response: %v", ErrHandshakeFailed, err)
	}
	if err = bufrw.Flush(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: failed to flush handshake response: %v", ErrHandshakeFailed, err)
	}

	return NewConn(conn, true, opts.extensions), nil
}

// computeAcceptKey computes the Sec-WebSocket-Accept value.
func computeAcceptKey(challengeKey string) string {
	h := sha1.New()
	h.Write([]byte(challengeKey))
	h.Write([]byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// generateKey generates a random base64-encoded string for the Sec-WebSocket-Key header.
func generateKey() (string, error) {
	key := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("%w: %v", ErrFailedToGenerateKey, err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// headerContains checks if a header contains a specific value,
// based on case-insensitive comparison to allow for case variations,
// and handling of multiple values separated by commas.
func headerContains(h http.Header, name string, value string) bool {
	for _, s := range h[name] {
		for _, v := range strings.Split(s, ",") {
			if strings.EqualFold(strings.TrimSpace(v), value) {
				return true
			}
		}
	}
	return false
}

// xor XORs the payload with the masking key,
// updating the payload in place to avoid additional
// allocations.
//
// https://tools.ietf.org/html/rfc6455#section-5.3
func xor(key []byte, data []byte) {
	for i := range data {
		data[i] ^= key[i%4]
	}
}
