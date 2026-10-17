package wsjson_test

import (
	"context"
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
