//go:build js && wasm

// Command client is the WebAssembly half of the browser example. It dials the
// server's /echo endpoint with [browser.Dial] and prints every reply.
//
// In a browser it reads messages from the page's form and appends replies to
// the page. Under Node.js, which has no document, it sends its command-line
// arguments to the URL in $ECHO_URL and exits once every reply has arrived.
// The example's test uses that mode.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall/js"
	"time"

	"github.com/picatz/websocket"
	"github.com/picatz/websocket/browser"
)

func main() {
	document := js.Global().Get("document")
	if document.IsUndefined() {
		if err := runArgs(os.Getenv("ECHO_URL"), os.Args[1:]); err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		return
	}
	runPage(document)
}

// runPage wires the page's form to a connection and never returns, so the
// form's event handler stays alive.
func runPage(document js.Value) {
	logEl := document.Call("getElementById", "log")
	show := func(line string) {
		logEl.Set("textContent", logEl.Get("textContent").String()+line+"\n")
	}
	logEl.Set("textContent", "")

	location := js.Global().Get("location")
	scheme := "ws:"
	if location.Get("protocol").String() == "https:" {
		scheme = "wss:"
	}
	url := scheme + "//" + location.Get("host").String() + "/echo"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := browser.Dial(ctx, url)
	cancel()
	if err != nil {
		show("dial: " + err.Error())
		select {}
	}
	show("connected to " + url)

	// The event handler runs on the JavaScript event loop and must not
	// block, so it hands each message to a writer goroutine.
	outgoing := make(chan string, 16)
	go func() {
		for message := range outgoing {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := conn.WriteMessageContext(ctx, websocket.TextMessage, []byte(message))
			cancel()
			if err != nil {
				show("write: " + err.Error())
				return
			}
		}
	}()

	input := document.Call("getElementById", "message")
	form := document.Call("getElementById", "send")
	submit := js.FuncOf(func(this js.Value, args []js.Value) any {
		args[0].Call("preventDefault")
		message := input.Get("value").String()
		if message == "" {
			return nil
		}
		input.Set("value", "")
		select {
		case outgoing <- message:
			show("> " + message)
		default:
			show("send queue full; dropped " + message)
		}
		return nil
	})
	form.Call("addEventListener", "submit", submit)
	input.Set("disabled", false)
	form.Call("querySelector", "button").Set("disabled", false)
	input.Call("focus")

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			show(describeClose(err))
			break
		}
		show("< " + string(data))
	}
	input.Set("disabled", true)
	select {}
}

// runArgs sends each message to url and prints each reply.
func runArgs(url string, messages []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := browser.Dial(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, message := range messages {
		if err := conn.WriteMessageContext(ctx, websocket.TextMessage, []byte(message)); err != nil {
			return err
		}
		_, data, err := conn.ReadMessageContext(ctx)
		if err != nil {
			return err
		}
		fmt.Println("< " + string(data))
	}
	return conn.CloseWithStatus(websocket.StatusNormalClosure, "done")
}

// describeClose turns a read error into a line for the page.
func describeClose(err error) string {
	if errors.Is(err, io.EOF) {
		return fmt.Sprintf("closed by server (status %d)", websocket.CloseStatus(err))
	}
	return "read: " + err.Error()
}
