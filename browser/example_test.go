//go:build js && wasm

package browser_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/picatz/websocket"
	"github.com/picatz/websocket/browser"
)

// Dial connects through the host's WebSocket object, and the connection is
// then used like a websocket.Conn: one goroutine reads until ReadMessage
// returns an error, and a normal close by the server ends the loop.
func ExampleDial() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := browser.Dial(ctx, "wss://example.com/socket")
	if err != nil {
		log.Fatal(err) // matches browser.ErrDialFailed; the host gives no reason
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		log.Fatal(err)
	}
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			fmt.Println("closed with", websocket.CloseStatus(err))
			return
		}
		fmt.Printf("%s\n", data)
	}
}

// A Dialer offers subprotocols and bounds incoming messages. The host chooses
// everything else about the handshake.
func ExampleDialer() {
	dialer := &browser.Dialer{
		Subprotocols:   []string{"chat.v2", "chat.v1"},
		MaxMessageSize: 64 << 10,
	}
	conn, err := dialer.Dial(context.Background(), "wss://example.com/chat")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.CloseWithStatus(websocket.StatusNormalClosure, "bye")
	fmt.Println("speaking", conn.Subprotocol())
}
