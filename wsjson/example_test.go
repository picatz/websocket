package wsjson_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/picatz/websocket"
	"github.com/picatz/websocket/wsjson"
)

type greeting struct {
	Name string `json:"name"`
}

type reply struct {
	Message string `json:"message"`
}

// A server that answers each JSON request with a JSON reply.
func Example() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		ctx := r.Context()
		for {
			var g greeting
			if err := wsjson.Read(ctx, conn, &g); err != nil {
				return
			}
			if err := wsjson.Write(ctx, conn, reply{"hello, " + g.Name}); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if err := wsjson.Write(ctx, conn, greeting{"gopher"}); err != nil {
		log.Fatal(err)
	}
	var rep reply
	if err := wsjson.Read(ctx, conn, &rep); err != nil {
		log.Fatal(err)
	}
	fmt.Println(rep.Message)
	// Output: hello, gopher
}

// Read rejects a binary message with an error matching ErrNotText and a
// malformed one with the decoding error. Neither closes the connection, so
// the caller decides whether to skip the message or end the session.
func ExampleRead() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(context.Background(), websocket.BinaryMessage, []byte{0xca, 0xfe})
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(`{"name":`))
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(`{"name":"gopher"}`))
		_, _, _ = conn.ReadMessage(context.Background())
	}))
	defer srv.Close()

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	for range 3 {
		var g greeting
		err := wsjson.Read(ctx, conn, &g)
		var syntaxErr *json.SyntaxError
		switch {
		case errors.Is(err, wsjson.ErrNotText):
			fmt.Println("skipped a binary message")
		case errors.As(err, &syntaxErr):
			fmt.Println("skipped malformed JSON")
		case err != nil:
			log.Fatal(err)
		default:
			fmt.Println("hello,", g.Name)
		}
	}
	// Output:
	// skipped a binary message
	// skipped malformed JSON
	// hello, gopher
}
