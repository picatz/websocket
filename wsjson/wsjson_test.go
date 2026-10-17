package wsjson_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"github.com/picatz/websocket"
	"github.com/picatz/websocket/wsjson"
)

func pipe(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	a, b := net.Pipe()
	server, client = websocket.NewConn(a, true, nil), websocket.NewConn(b, false, nil)
	// Closing the pipe ends directly skips the one-second close handshake.
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return server, client
}

type event struct {
	Kind string `json:"kind"`
	N    int    `json:"n"`
}

func TestRoundTrip(t *testing.T) {
	server, client := pipe(t)
	errc := make(chan error, 1)
	go func() { errc <- wsjson.Write(t.Context(), client, event{"tick", 7}) }()

	var got event
	if err := wsjson.Read(t.Context(), server, &got); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if got != (event{"tick", 7}) {
		t.Fatalf("got %+v", got)
	}
}

func TestReadRejectsBinary(t *testing.T) {
	server, client := pipe(t)
	go func() { _ = client.WriteMessage(websocket.BinaryMessage, []byte(`{}`)) }()
	var v map[string]any
	if err := wsjson.Read(t.Context(), server, &v); !errors.Is(err, wsjson.ErrNotText) {
		t.Fatalf("Read = %v, want ErrNotText", err)
	}
}

func TestReadDecodeError(t *testing.T) {
	server, client := pipe(t)
	go func() { _ = client.WriteMessage(websocket.TextMessage, []byte(`{"n":"x"}`)) }()
	var v event
	err := wsjson.Read(t.Context(), server, &v)
	if _, ok := errors.AsType[*json.UnmarshalTypeError](err); !ok {
		t.Fatalf("Read = %v, want an UnmarshalTypeError", err)
	}
}

func TestWriteEncodeErrorSendsNothing(t *testing.T) {
	_, client := pipe(t)
	// No reader on the pipe: any I/O would block, so returning proves none ran.
	if err := wsjson.Write(t.Context(), client, func() {}); err == nil {
		t.Fatal("encoded a func")
	}
}

func TestReadContextAlreadyDone(t *testing.T) {
	server, _ := pipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var v event
	if err := wsjson.Read(ctx, server, &v); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read = %v, want context.Canceled", err)
	}
}
