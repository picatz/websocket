//go:build js && wasm

package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall/js"
	"time"
	"unicode/utf8"

	"github.com/picatz/websocket"
)

// ErrDialFailed reports that the host could not open the connection. The host
// deliberately does not say why: a refused connection, a TLS failure and a
// rejected handshake look the same to the program.
var ErrDialFailed = errors.New("browser: websocket dial failed")

// errClosed is returned by operations on a closed or closing connection. Like
// websocket.Conn's, it matches net.ErrClosed.
var errClosed error = closedError{}

type closedError struct{}

func (closedError) Error() string { return "browser: use of closed connection" }

// Is reports whether target is net.ErrClosed.
func (closedError) Is(target error) bool { return target == net.ErrClosed }

// readyState values of the host WebSocket object.
//
// https://websockets.spec.whatwg.org/#dom-websocket-readystate
const (
	stateConnecting = 0
	stateOpen       = 1
)

// A Dialer contains options for connecting to a WebSocket server through the
// host. Its zero value offers no subprotocol and applies
// websocket.DefaultMaxMessageSize. A Dialer may be reused and is safe for
// concurrent use; Dial does not modify it.
//
// The host controls request headers, TLS, proxies and extensions, so a Dialer
// has none of websocket.Dialer's fields for them.
type Dialer struct {
	// Subprotocols are the application subprotocols to offer, in order of
	// preference. The host rejects invalid or duplicate tokens, and Dial then
	// fails. Conn.Subprotocol reports the server's selection.
	Subprotocols []string

	// MaxMessageSize bounds each incoming message in bytes. Zero means
	// websocket.DefaultMaxMessageSize; a negative value means no limit. A
	// larger message closes the connection and ReadMessage returns an error
	// matching websocket.ErrPayloadTooLarge. The close carries no status
	// code: hosts only let programs send 1000 and 3000 through 4999, not 1009
	// (Message Too Big). The host has already received the whole message by
	// then, so the limit bounds what the program keeps, not what the host
	// buffers.
	MaxMessageSize int
}

// message is one received data message.
type message struct {
	opcode websocket.Opcode
	data   []byte
}

// Conn is a WebSocket connection driven by the host's WebSocket object.
//
// ReadMessage may run concurrently with WriteMessage, and Close may be called
// concurrently with either.
type Conn struct {
	ws       js.Value
	maxBytes int

	// readMu serializes readers so that each message has one recipient.
	readMu sync.Mutex
	// writeMu serializes writers, so that messages keep their order while
	// waiting for the host's send buffer to drain.
	writeMu sync.Mutex

	// mu guards the fields below. It is never held across a call into the
	// host, which may dispatch events, and so re-enter Conn, synchronously.
	mu sync.Mutex
	// queue holds messages the host delivered that no reader has taken yet.
	queue []message
	// ready is signaled, without blocking, whenever queue grows or err is set.
	ready chan struct{}
	// err is the terminal read error, set once the connection ends.
	err error
	// closed records a local Close.
	closed bool

	// done is closed after the host's close event, once err is set.
	done      chan struct{}
	listeners []listener
	release   sync.Once
}

// listener is an event handler registered on the host object.
type listener struct {
	event string
	f     js.Func
}

// Dial opens a WebSocket connection to urlStr, a ws or wss URL; browsers also
// accept http and https URLs and URLs relative to the document. It waits for the
// host to finish the opening handshake, or for ctx to end, in which case the
// connection attempt is abandoned. Canceling ctx after Dial returns has no
// effect on the connection.
func (d *Dialer) Dial(ctx context.Context, urlStr string) (conn *Conn, err error) {
	if d == nil {
		d = &Dialer{}
	}
	maxBytes := d.MaxMessageSize
	switch {
	case maxBytes == 0:
		maxBytes = websocket.DefaultMaxMessageSize
	case maxBytes < 0:
		maxBytes = 0
	}

	ws, err := newWebSocket(urlStr, d.Subprotocols)
	if err != nil {
		return nil, err
	}
	ws.Set("binaryType", "arraybuffer")

	c := &Conn{
		ws:       ws,
		maxBytes: maxBytes,
		ready:    make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	opened, failed := make(chan struct{}), make(chan struct{})
	var failOnce sync.Once
	c.on("open", func(js.Value) { close(opened) })
	c.on("message", c.onMessage)
	c.on("close", c.onClose)
	// The error event carries no detail. The standard follows it with close,
	// but some hosts (Node.js 22) never fire close for a rejected handshake,
	// so an error before open ends the dial by itself.
	c.on("error", func(js.Value) {
		failOnce.Do(func() { close(failed) })
		c.onError()
	})

	select {
	case <-opened:
		return c, nil
	case <-failed:
	case <-c.done:
	case <-ctx.Done():
		err = context.Cause(ctx)
	}
	c.terminate()
	c.releaseListeners()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrDialFailed, urlStr, err)
	}
	return nil, fmt.Errorf("%w: %s", ErrDialFailed, urlStr)
}

// Dial connects to urlStr with a zero Dialer.
func Dial(ctx context.Context, urlStr string) (*Conn, error) {
	return (&Dialer{}).Dial(ctx, urlStr)
}

// newWebSocket constructs the host object, turning the exception thrown for a
// malformed URL or subprotocol list into an error.
func newWebSocket(urlStr string, subprotocols []string) (ws js.Value, err error) {
	constructor := js.Global().Get("WebSocket")
	if constructor.IsUndefined() {
		return js.Value{}, fmt.Errorf("%w: host has no WebSocket", ErrDialFailed)
	}
	defer func() {
		if r := recover(); r != nil {
			jsErr, ok := r.(js.Error)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("%w: %w", ErrDialFailed, jsErr)
		}
	}()
	protocols := make([]any, len(subprotocols))
	for i, p := range subprotocols {
		protocols[i] = p
	}
	return constructor.New(urlStr, protocols), nil
}

// on registers handler for a host event. The handler runs on the host's event
// loop and must not block.
func (c *Conn) on(event string, handler func(js.Value)) {
	f := js.FuncOf(func(_ js.Value, args []js.Value) any {
		handler(args[0])
		return nil
	})
	c.listeners = append(c.listeners, listener{event, f})
	c.ws.Call("addEventListener", event, f)
}

func (c *Conn) onMessage(ev js.Value) {
	data := ev.Get("data")
	var m message
	if data.Type() == js.TypeString {
		m = message{opcode: websocket.TextMessage, data: []byte(data.String())}
	} else {
		array := js.Global().Get("Uint8Array").New(data)
		m = message{opcode: websocket.BinaryMessage, data: make([]byte, array.Length())}
		js.CopyBytesToGo(m.data, array)
	}

	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	if c.maxBytes > 0 && len(m.data) > c.maxBytes {
		c.err = fmt.Errorf("%w: message of %d bytes exceeds limit of %d", websocket.ErrPayloadTooLarge, len(m.data), c.maxBytes)
		c.signal()
		c.mu.Unlock()
		c.ws.Call("close")
		return
	}
	c.queue = append(c.queue, m)
	c.signal()
	c.mu.Unlock()
}

func (c *Conn) onClose(ev js.Value) {
	c.mu.Lock()
	if c.err == nil {
		c.err = closeEventError(ev, c.closed)
	}
	c.signal()
	c.mu.Unlock()

	close(c.done)
	// Release once this callback, itself one of the listeners, has returned.
	go c.releaseListeners()
}

// closeEventError converts a close event into ReadMessage's terminal error.
// After a local Close it matches net.ErrClosed. A clean close from the peer is a
// *websocket.CloseError carrying the peer's code and reason, which matches
// io.EOF like websocket.Conn's. A close without a closing handshake, which the
// host reports as code 1006 (Abnormal Closure), matches io.ErrUnexpectedEOF.
//
// https://websockets.spec.whatwg.org/#the-closeevent-interface
func closeEventError(ev js.Value, closedLocally bool) error {
	if closedLocally {
		return errClosed
	}
	code := websocket.StatusCode(ev.Get("code").Int())
	if !ev.Get("wasClean").Bool() || code == websocket.StatusAbnormalClosure {
		return fmt.Errorf("browser: connection closed without a closing handshake: %w", io.ErrUnexpectedEOF)
	}
	return &websocket.CloseError{Code: code, Reason: ev.Get("reason").String()}
}

// releaseListeners detaches and frees the event handlers. Once detached, the
// host no longer calls them, so releasing them is safe.
func (c *Conn) releaseListeners() {
	c.release.Do(func() {
		for _, l := range c.listeners {
			c.ws.Call("removeEventListener", l.event, l.f)
			l.f.Release()
		}
	})
}

// onError ends reading after the host fails an open connection, without
// waiting for a close event that may not come.
func (c *Conn) onError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil && !c.closed {
		c.err = fmt.Errorf("browser: connection failed: %w", io.ErrUnexpectedEOF)
		c.signal()
	}
}

// signal wakes a waiting reader. c.mu is held.
func (c *Conn) signal() {
	select {
	case c.ready <- struct{}{}:
	default:
	}
}

// ReadMessage returns the next data message. Messages that arrived before the
// connection ended are returned first. Then ReadMessage returns a
// *websocket.CloseError carrying the peer's code and reason if the peer closed
// the connection, which matches io.EOF with errors.Is; an error matching net.ErrClosed after
// Close; an error matching websocket.ErrPayloadTooLarge if a message exceeded
// the limit; or one matching io.ErrUnexpectedEOF if the host reported a
// failure or the connection closed without a closing handshake.
func (c *Conn) ReadMessage() (messageType websocket.Opcode, data []byte, err error) {
	return c.ReadMessageContext(context.Background())
}

// ReadMessageContext is ReadMessage bounded by ctx. If ctx ends before a
// message is available, the connection is closed, as with
// websocket.Conn.ReadMessageContext, and the error wraps context.Cause(ctx).
func (c *Conn) ReadMessageContext(ctx context.Context) (messageType websocket.Opcode, data []byte, err error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		c.mu.Lock()
		if len(c.queue) > 0 {
			m := c.queue[0]
			c.queue[0] = message{}
			c.queue = c.queue[1:]
			c.mu.Unlock()
			return m.opcode, m.data, nil
		}
		if c.err != nil || c.closed {
			err := c.err
			if c.closed {
				// After Close, do not wait for the host's close event.
				err = errClosed
			}
			c.mu.Unlock()
			return 0, nil, err
		}
		c.mu.Unlock()

		select {
		case <-c.ready:
		case <-ctx.Done():
			c.terminate()
			return 0, nil, fmt.Errorf("browser: read: %w", context.Cause(ctx))
		}
	}
}

// writeHighWaterMark is how many bytes the host may hold unsent before a
// write waits. The host offers no event when its buffer drains, so a waiting
// writer polls bufferedAmount, starting at writePollMin and backing off to
// writePollMax.
//
// https://websockets.spec.whatwg.org/#dom-websocket-bufferedamount
var writeHighWaterMark = 1 << 20

const (
	writePollMin = time.Millisecond
	writePollMax = 50 * time.Millisecond
)

// WriteMessage sends a text or binary message. Text must be valid UTF-8. It
// is WriteMessageContext without a deadline.
func (c *Conn) WriteMessage(messageType websocket.Opcode, data []byte) error {
	return c.WriteMessageContext(context.Background(), messageType, data)
}

// WriteMessageContext sends a text or binary message. Text must be valid
// UTF-8.
//
// The host queues outgoing messages without limit, so for backpressure the
// write first waits while more than 1 MiB is already queued and unsent. Then
// it hands the message to the host and returns; it does not wait for the
// network. If ctx ends while waiting, it returns an error wrapping
// context.Cause(ctx). Nothing has been sent at that point, so unlike
// websocket.Conn.WriteMessageContext the connection stays open. Writes are
// serialized, so concurrent writers keep their order and share the wait.
//
// It returns an error matching net.ErrClosed once the connection is closing
// or closed, where the host would silently discard the message.
func (c *Conn) WriteMessageContext(ctx context.Context, messageType websocket.Opcode, data []byte) error {
	var payload any
	switch messageType {
	case websocket.TextMessage:
		if !utf8.Valid(data) {
			// The host would replace invalid sequences instead of failing.
			return websocket.ErrInvalidFrame
		}
		payload = string(data)
	case websocket.BinaryMessage:
		array := js.Global().Get("Uint8Array").New(len(data))
		js.CopyBytesToJS(array, data)
		payload = array
	default:
		return websocket.ErrInvalidOpcode
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.waitForBuffer(ctx); err != nil {
		return err
	}
	c.ws.Call("send", payload)
	return nil
}

// waitForBuffer waits until the host's unsent bytes are at or below
// writeHighWaterMark and the connection is still open. c.writeMu is held.
func (c *Conn) waitForBuffer(ctx context.Context) error {
	var timer *time.Timer
	poll := writePollMin
	for {
		c.mu.Lock()
		ended := c.closed || c.err != nil
		c.mu.Unlock()
		if ended || c.ws.Get("readyState").Int() != stateOpen {
			return errClosed
		}
		if c.ws.Get("bufferedAmount").Int() <= writeHighWaterMark {
			return nil
		}
		if timer == nil {
			timer = time.NewTimer(poll)
			defer timer.Stop()
		} else {
			timer.Reset(poll)
		}
		select {
		case <-timer.C:
		case <-c.done:
		case <-ctx.Done():
			return fmt.Errorf("browser: write: %w", context.Cause(ctx))
		}
		poll = min(2*poll, writePollMax)
	}
}

// Subprotocol returns the subprotocol the server selected, or "" if none.
func (c *Conn) Subprotocol() string {
	return c.ws.Get("protocol").String()
}

// Close starts the closing handshake with status 1000 (Normal Closure). Unlike
// websocket.Conn.Close it does not wait or block; the host completes the
// handshake. Messages already received stay readable. Repeated calls return
// websocket.ErrAlreadyClosed.
func (c *Conn) Close() error {
	return c.close(websocket.StatusNormalClosure, "")
}

// CloseWithStatus is Close with an explicit status code and reason. Hosts only
// let programs send 1000 (Normal Closure) and 3000 through 4999, a narrower
// range than websocket.Conn.CloseWithStatus accepts. The reason must be valid
// UTF-8 of at most 123 bytes. Otherwise it returns an error matching
// websocket.ErrInvalidCloseStatus and leaves the connection open.
//
// https://websockets.spec.whatwg.org/#dom-websocket-close
func (c *Conn) CloseWithStatus(code websocket.StatusCode, reason string) error {
	if code != websocket.StatusNormalClosure && (code < 3000 || code > 4999) {
		return fmt.Errorf("%w: hosts only send 1000 and 3000-4999, not %d", websocket.ErrInvalidCloseStatus, int(code))
	}
	if len(reason) > maxCloseReason {
		return fmt.Errorf("%w: reason is %d bytes, limit is %d", websocket.ErrInvalidCloseStatus, len(reason), maxCloseReason)
	}
	if !utf8.ValidString(reason) {
		return fmt.Errorf("%w: reason is not valid UTF-8", websocket.ErrInvalidCloseStatus)
	}
	return c.close(code, reason)
}

// maxCloseReason is the largest close reason that fits in a control frame
// after the two-byte status code.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-5.5
const maxCloseReason = 125 - 2

func (c *Conn) close(code websocket.StatusCode, reason string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return websocket.ErrAlreadyClosed
	}
	c.closed = true
	open := c.err == nil
	c.mu.Unlock()
	if open {
		c.ws.Call("close", int(code), reason)
	}
	return nil
}

// terminate closes the connection without a status code, for abandoned dials
// and canceled reads.
func (c *Conn) terminate() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	if state := c.ws.Get("readyState").Int(); state == stateConnecting || state == stateOpen {
		c.ws.Call("close")
	}
}
