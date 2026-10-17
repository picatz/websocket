package websocket

import (
	"context"
	"errors"
	"testing"
)

// TestProtocolErrorsMatchErrProtocol checks that one errors.Is check covers
// every message a connection refuses, and that the caller's own mistakes do
// not look like the peer's.
func TestProtocolErrorsMatchErrProtocol(t *testing.T) {
	for _, err := range []error{
		ErrMessageTooLarge,
		errInvalidFrame,
		errUnexpectedFrame,
		errUnexpectedContinuation,
		errControlFrameFragment,
		errUnmaskedFrame,
		errMaskedFrame,
		errUnsupportedExtensions,
		errInvalidOpcode,
		errInvalidCompressedPayload,
		&compressedPayloadError{cause: errors.New("flate: corrupt input")},
		failWith(StatusProtocolError, errInvalidFrame),
	} {
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("%v does not match ErrProtocol", err)
		}
	}
	for _, err := range []error{errInvalidUTF8, errInvalidMessageType, errClosed} {
		if errors.Is(err, ErrProtocol) {
			t.Errorf("%v matches ErrProtocol", err)
		}
	}
}

// TestReadProtocolViolationMatchesErrProtocol checks the error ReadMessage
// returns for a peer's frame with a reserved opcode.
func TestReadProtocolViolationMatchesErrProtocol(t *testing.T) {
	c := frameConn([]byte{0x83, 0x00}, false, 0)
	if _, _, err := c.ReadMessage(context.Background()); !errors.Is(err, ErrProtocol) {
		t.Fatalf("ReadMessage = %v, want a match for ErrProtocol", err)
	}
}
