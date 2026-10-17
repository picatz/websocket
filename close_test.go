package websocket

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCloseWithStatusWire(t *testing.T) {
	reason := strings.Repeat("é", maxCloseReason/2) // 122 bytes of multibyte UTF-8
	tests := []struct {
		code   StatusCode
		reason string
	}{
		{StatusNormalClosure, ""},
		{StatusGoingAway, "shutting down"},
		{StatusPolicyViolation, reason},
		{StatusServiceRestart, ""}, // IANA registry
		{3000, "registered"},
		{4999, strings.Repeat("x", maxCloseReason)},
	}
	for _, server := range []bool{false, true} {
		for _, tt := range tests {
			raw := &memoryConn{Reader: bytes.NewReader(nil)}
			c := NewConn(raw, ConnConfig{Server: server})
			if err := c.CloseWithStatus(tt.code, tt.reason); err != nil {
				t.Fatalf("server=%v %v: %v", server, tt.code, err)
			}
			got := failureClosePayload(t, raw.written.Bytes(), server)
			if ce := newCloseError(got); ce.Code != tt.code || ce.Reason != tt.reason {
				t.Fatalf("server=%v: wire close %v %q, want %v %q", server, ce.Code, ce.Reason, tt.code, tt.reason)
			}
			if err := c.CloseWithStatus(tt.code, tt.reason); !errors.Is(err, ErrAlreadyClosed) {
				t.Fatalf("second close: %v", err)
			}
		}
	}
}

func TestCloseSendsNormalClosure(t *testing.T) {
	raw := &memoryConn{Reader: bytes.NewReader(nil)}
	if err := NewConn(raw, ConnConfig{Server: true}).Close(); err != nil {
		t.Fatal(err)
	}
	if got := failureClosePayload(t, raw.written.Bytes(), true); !bytes.Equal(got, []byte{3, 232}) {
		t.Fatalf("Close payload = %x, want 03e8", got)
	}
}

// Invalid statuses are caller errors: nothing is written and the connection
// stays usable.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.2
func TestCloseWithStatusRejectsInvalid(t *testing.T) {
	tests := []struct {
		code   StatusCode
		reason string
	}{
		{-1, ""},
		{0, ""},
		{999, ""},
		{1004, ""}, // reserved
		{StatusNoStatusReceived, ""},
		{StatusAbnormalClosure, ""},
		{StatusTLSHandshake, ""},
		{1016, ""},
		{2999, ""},
		{5000, ""},
		{StatusNormalClosure, strings.Repeat("x", maxCloseReason+1)},
		{StatusNormalClosure, "\xff"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d/%d", tt.code, len(tt.reason)), func(t *testing.T) {
			raw := &memoryConn{Reader: bytes.NewReader(nil)}
			c := NewConn(raw, ConnConfig{})
			if err := c.CloseWithStatus(tt.code, tt.reason); !errors.Is(err, ErrInvalidCloseStatus) {
				t.Fatalf("error = %v, want ErrInvalidCloseStatus", err)
			}
			if raw.written.Len() != 0 {
				t.Fatalf("wrote %x for an invalid status", raw.written.Bytes())
			}
			if err := c.WriteMessage(TextMessage, []byte("still open")); err != nil {
				t.Fatalf("write after rejected close: %v", err)
			}
		})
	}
}

func TestReadMessageReturnsPeerCloseError(t *testing.T) {
	for _, server := range []bool{false, true} {
		for _, tt := range []struct {
			payload []byte
			want    CloseError
		}{
			{nil, CloseError{Code: StatusNoStatusReceived}},
			{[]byte{3, 232}, CloseError{Code: StatusNormalClosure}},
			{append([]byte{0x0f, 0xa0}, "bye"...), CloseError{Code: 4000, Reason: "bye"}},
		} {
			// An unfinished fragmented message does not hide the close status.
			wire := append(failureWire(server, 0x02, []byte("a")), failureWire(server, 0x88, tt.payload)...)
			raw := &memoryConn{Reader: bytes.NewReader(wire)}
			c := NewConn(raw, ConnConfig{Server: server})
			_, data, err := c.ReadMessage()
			ce, ok := errors.AsType[*CloseError](err)
			if !ok || *ce != tt.want || data != nil {
				t.Fatalf("server=%v: ReadMessage = %q, %v; want %+v", server, data, err, tt.want)
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("CloseError does not match io.EOF")
			}
			if got := CloseStatus(err); got != tt.want.Code {
				t.Fatalf("CloseStatus = %v, want %v", got, tt.want.Code)
			}
			// The echo carries the peer's payload unchanged.
			if got := failureClosePayload(t, raw.written.Bytes(), server); !bytes.Equal(got, tt.payload) {
				t.Fatalf("echo = %x, want %x", got, tt.payload)
			}
			if _, _, err := c.ReadMessage(); !errors.Is(err, net.ErrClosed) || CloseStatus(err) != -1 {
				t.Fatalf("read after peer close: %v", err)
			}
		}
	}
}

// A close frame interleaved in a compressed message takes the streaming
// decoder's path, which must surface the same CloseError.
func TestReadMessageReturnsPeerCloseErrorDuringCompression(t *testing.T) {
	compressed := defaultStoredDeflate([]byte("partial"))
	wire := append(failureWire(false, 0x42, compressed), failureWire(false, 0x88, append([]byte{3, 233}, "away"...))...)
	c := NewConn(&memoryConn{Reader: bytes.NewReader(wire)}, ConnConfig{Extensions: []Extension{defaultLimitPMD(t, false)}})
	_, _, err := c.ReadMessage()
	if ce, ok := errors.AsType[*CloseError](err); !ok || ce.Code != StatusGoingAway || ce.Reason != "away" {
		t.Fatalf("ReadMessage = %v, want StatusGoingAway close", err)
	}
}

func TestCloseStatus(t *testing.T) {
	ce := &CloseError{Code: StatusGoingAway, Reason: "bye"}
	for _, tt := range []struct {
		err  error
		want StatusCode
	}{
		{nil, -1},
		{io.EOF, -1},
		{errors.New("other"), -1},
		{ce, StatusGoingAway},
		{fmt.Errorf("read: %w", ce), StatusGoingAway},
		{errors.Join(io.ErrUnexpectedEOF, ce), StatusGoingAway},
	} {
		if got := CloseStatus(tt.err); got != tt.want {
			t.Errorf("CloseStatus(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
	if got, want := ce.Error(), `websocket: peer closed with status 1001 (StatusGoingAway): "bye"`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got, want := (&CloseError{Code: 4000}).Error(), "websocket: peer closed with status 4000 (StatusCode(4000))"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// End to end over a synchronous pipe: each side observes the other's status,
// including through ReadMessageContext.
func TestCloseWithStatusRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	server := NewConn(a, ConnConfig{Server: true})
	client := NewConn(b, ConnConfig{})
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	done := make(chan error, 1)
	go func() { done <- server.CloseWithStatus(StatusPolicyViolation, "quota exceeded") }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := client.ReadMessageContext(ctx)
	if ce, ok := errors.AsType[*CloseError](err); !ok || ce.Code != StatusPolicyViolation || ce.Reason != "quota exceeded" {
		t.Fatalf("client read = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server close: %v", err)
	}
}

func TestStatusCodeString(t *testing.T) {
	for code, want := range map[StatusCode]string{
		StatusNormalClosure:    "StatusNormalClosure",
		StatusNoStatusReceived: "StatusNoStatusReceived",
		StatusTLSHandshake:     "StatusTLSHandshake",
		StatusServiceRestart:   "StatusServiceRestart",
		StatusTryAgainLater:    "StatusTryAgainLater",
		StatusBadGateway:       "StatusBadGateway",
		4000:                   "StatusCode(4000)",
		-1:                     "StatusCode(-1)",
	} {
		if got := code.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(code), got, want)
		}
	}
}
