package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/picatz/websocket"
)

func dial(t *testing.T) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, typ websocket.Opcode, msg string) {
	t.Helper()
	if err := conn.WriteMessage(typ, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCalls(t *testing.T) {
	conn := dial(t)
	for _, tc := range []struct {
		name string
		typ  websocket.Opcode
		req  string
		want string
	}{
		{"sum", websocket.TextMessage,
			`{"jsonrpc":"2.0","id":1,"method":"sum","params":[1,2,3.5]}`,
			`{"jsonrpc":"2.0","id":1,"result":6.5}`},
		{"zero result", websocket.TextMessage,
			`{"jsonrpc":"2.0","id":"a","method":"sum","params":[]}`,
			`{"jsonrpc":"2.0","id":"a","result":0}`},
		{"invalid params", websocket.TextMessage,
			`{"jsonrpc":"2.0","id":2,"method":"sum","params":{"a":1}}`,
			`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"params must be an array of numbers"}}`},
		{"unknown method", websocket.TextMessage,
			`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
			`{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found: nope"}}`},
		{"parse error", websocket.TextMessage,
			`{"jsonrpc":`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"invalid JSON"}}`},
		{"batch", websocket.TextMessage,
			`[{"jsonrpc":"2.0","id":4,"method":"sum","params":[1]}]`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"not a request object"}}`},
		{"wrong version", websocket.TextMessage,
			`{"jsonrpc":"1.0","id":5,"method":"sum","params":[1]}`,
			`{"jsonrpc":"2.0","id":5,"error":{"code":-32600,"message":"want \"jsonrpc\":\"2.0\" and a method"}}`},
		{"binary", websocket.BinaryMessage,
			`{"jsonrpc":"2.0","id":6,"method":"sum","params":[1]}`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"requests must be text messages"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			send(t, conn, tc.typ, tc.req)
			if got := receive(t, conn); got != tc.want {
				t.Fatalf("response:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestConcurrentCalls checks that a slow call does not hold up a fast one
// and that a notification gets no response.
func TestConcurrentCalls(t *testing.T) {
	conn := dial(t)
	send(t, conn, websocket.TextMessage, `{"jsonrpc":"2.0","id":"slow","method":"sleep","params":[200]}`)
	send(t, conn, websocket.TextMessage, `{"jsonrpc":"2.0","method":"sum","params":[1]}`)
	send(t, conn, websocket.TextMessage, `{"jsonrpc":"2.0","id":"fast","method":"sum","params":[1]}`)

	for _, want := range []string{
		`{"jsonrpc":"2.0","id":"fast","result":1}`,
		`{"jsonrpc":"2.0","id":"slow","result":200}`,
	} {
		if got := receive(t, conn); got != want {
			t.Fatalf("response:\n got %s\nwant %s", got, want)
		}
	}
}
