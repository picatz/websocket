package websocket

import (
	"encoding/binary"
	"errors"
	"testing"
	"unicode/utf8"
)

// sendableCloseCode reports whether an endpoint may put code in a Close frame,
// written out from RFC 6455 Section 7.4 independently of the package's own
// table: 1004, 1005, 1006 and 1015 are reserved for local use, and 1016 to 2999
// are unassigned.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-7.4.1
func sendableCloseCode(code uint16) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1014:
		return true
	case code >= 3000 && code <= 4999:
		return true
	}
	return false
}

// FuzzClosePayload checks every Close frame body against RFC 6455 Section
// 5.5.1: a valid body is reported as a *CloseError carrying the peer's code
// and reason and is answered with a Close frame, and an invalid one fails the
// connection with ErrProtocol and is answered with StatusProtocolError, or
// StatusInvalidFramePayloadData for a reason that is not UTF-8.
// CloseWithStatus must agree on which bodies are valid.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.5.1
func FuzzClosePayload(f *testing.F) {
	for _, seed := range [][]byte{
		nil, {3, 232}, {3, 232, 'b', 'y', 'e'}, {3}, {3, 237}, {3, 238},
		{3, 247}, {7, 208}, {11, 184}, {19, 135}, {19, 136}, {3, 232, 0xff},
		{3, 232, 0xe2, 0x82}, append([]byte{3, 232}, make([]byte, 123)...),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 125 {
			payload = payload[:125]
		}
		valid := len(payload) == 0 ||
			len(payload) >= 2 && sendableCloseCode(binary.BigEndian.Uint16(payload)) && utf8.Valid(payload[2:])

		wire := append([]byte{0x88, byte(len(payload))}, payload...)
		c := frameConn(wire, false, 0)
		_, _, err := c.ReadMessage()
		raw := c.conn.(*memoryConn)
		var ce *CloseError
		switch {
		case valid && len(payload) == 0:
			if !errors.As(err, &ce) || ce.Code != StatusNoStatusReceived || ce.Reason != "" {
				t.Fatalf("empty Close: err = %v, want StatusNoStatusReceived", err)
			}
		case valid:
			code := StatusCode(binary.BigEndian.Uint16(payload))
			if !errors.As(err, &ce) || ce.Code != code || ce.Reason != string(payload[2:]) {
				t.Fatalf("Close %x: err = %v, want code %d reason %q", payload, err, code, payload[2:])
			}
		default:
			if !errors.Is(err, ErrProtocol) || CloseStatus(err) != -1 {
				t.Fatalf("Close %x: err = %v, want ErrProtocol", payload, err)
			}
			// A reason that is not UTF-8 is invalid payload data (RFC 6455
			// Section 8.1); anything else is a protocol error.
			want := StatusProtocolError
			if len(payload) >= 2 && sendableCloseCode(binary.BigEndian.Uint16(payload)) {
				want = StatusInvalidFramePayloadData
			}
			reply := failureClosePayload(t, raw.written.Bytes(), false)
			if len(reply) < 2 || StatusCode(binary.BigEndian.Uint16(reply)) != want {
				t.Fatalf("Close %x answered with %x, want %v", payload, reply, want)
			}
		}
		if valid {
			// The reply is itself a Close frame the peer would accept.
			reply := failureClosePayload(t, raw.written.Bytes(), false)
			if len(reply) == 1 || len(reply) >= 2 && !sendableCloseCode(binary.BigEndian.Uint16(reply)) {
				t.Fatalf("Close %x answered with invalid %x", payload, reply)
			}
		}

		// Sending: CloseWithStatus accepts exactly the bodies a peer may send.
		if len(payload) < 2 {
			return
		}
		code, reason := StatusCode(binary.BigEndian.Uint16(payload)), string(payload[2:])
		err = frameConn(nil, false, 0).CloseWithStatus(code, reason)
		if valid != !errors.Is(err, ErrInvalidCloseStatus) {
			t.Fatalf("CloseWithStatus(%d, %q) = %v, want valid = %v", code, reason, err, valid)
		}
	})
}
