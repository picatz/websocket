// Command client is a command-line WebSocket client, in the spirit of wscat.
//
// It connects to a ws or wss URL, sends each line read from standard input as
// a text message, and prints each message it receives: text as is, binary as
// hexadecimal bytes. When standard input ends it starts the closing handshake
// and keeps printing until the server answers it, so piped input works:
//
//	echo hello | go run ./examples/client ws://localhost:8080/ws
//
// When the server closes first, the client exits with its status code and
// reason, and fails unless the server closed normally. An interrupt closes
// with StatusGoingAway.
//
// Flags:
//
//	-H "Name: value"   add a handshake request header; may be repeated
//	-subprotocol list  offer these comma-separated subprotocols
//	-compress          offer permessage-deflate compression
//	-timeout duration  bound the handshake (default 10s)
//
// HTTP_PROXY, HTTPS_PROXY and NO_PROXY select a proxy, as for HTTP clients.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/picatz/websocket"
)

// closeWait bounds the wait for the server's reply to the client's Close.
const closeWait = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "client:", err)
		}
		os.Exit(1)
	}
}

// headers collects repeated -H flags.
type headers http.Header

func (h headers) String() string { return "" }

func (h headers) Set(field string) error {
	name, value, ok := strings.Cut(field, ":")
	if !ok {
		return fmt.Errorf("header %q: want Name: value", field)
	}
	http.Header(h).Add(strings.TrimSpace(name), strings.TrimSpace(value))
	return nil
}

// run is the whole program, with its environment passed in so tests can
// drive it.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: client [flags] ws://host/path")
		fs.PrintDefaults()
	}
	header := headers{}
	fs.Var(header, "H", "add a handshake request `header`, as \"Name: value\"; may be repeated")
	subprotocols := fs.String("subprotocol", "", "offer these comma-separated subprotocols, in order of preference")
	compress := fs.Bool("compress", false, "offer permessage-deflate compression")
	timeout := fs.Duration("timeout", 10*time.Second, "bound the handshake")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return flag.ErrHelp
	}

	dialer := &websocket.Dialer{
		Header: http.Header(header),
		Proxy:  http.ProxyFromEnvironment,
	}
	if *subprotocols != "" {
		dialer.Subprotocols = strings.Split(*subprotocols, ",")
	}
	if *compress {
		dialer.Compression = &websocket.Compression{}
	}
	dialCtx, cancel := context.WithTimeout(ctx, *timeout)
	conn, resp, err := dialer.Dial(dialCtx, fs.Arg(0))
	cancel()
	if err != nil {
		if resp != nil {
			return fmt.Errorf("%w (server answered %s)", err, resp.Status)
		}
		return err
	}
	defer conn.Close()
	if p := conn.Subprotocol(); p != "" {
		fmt.Fprintln(stderr, "subprotocol:", p)
	}

	// Print every message until ReadMessage fails, which ends the session.
	received := make(chan error, 1)
	go func() {
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				received <- err
				return
			}
			if typ == websocket.BinaryMessage {
				fmt.Fprintf(stdout, "% x\n", data)
			} else {
				fmt.Fprintf(stdout, "%s\n", data)
			}
		}
	}()

	// Send every line. Standard input cannot be interrupted, so this
	// goroutine may outlive run when the server closes first.
	sent := make(chan error, 1)
	go func() {
		lines := bufio.NewScanner(stdin)
		for lines.Scan() {
			if err := conn.WriteMessage(websocket.TextMessage, lines.Bytes()); err != nil {
				sent <- err
				return
			}
		}
		sent <- lines.Err()
	}()

	select {
	case err := <-received:
		return closed(err, stderr)
	case err := <-sent:
		if err != nil {
			// A write fails once the server has closed; report the close.
			return closed(<-received, stderr)
		}
		return finish(conn, websocket.StatusNormalClosure, received, stderr)
	case <-ctx.Done():
		return finish(conn, websocket.StatusGoingAway, received, stderr)
	}
}

// finish closes the write side with code and keeps printing messages until
// the server's Close frame arrives, so replies already on their way are not
// lost. Conn.Shutdown would discard them. A server that does not answer within
// closeWait is cut off.
func finish(conn *websocket.Conn, code websocket.StatusCode, received <-chan error, stderr io.Writer) error {
	// CloseWrite fails if the server has closed meanwhile; its Close frame
	// then arrives below.
	_ = conn.CloseWrite(code, "")
	select {
	case err := <-received:
		return closed(err, stderr)
	case <-time.After(closeWait):
		return errors.New("server did not answer the closing handshake")
	}
}

// closed reports how the session ended. A normal closure is success.
func closed(err error, stderr io.Writer) error {
	var ce *websocket.CloseError
	if !errors.As(err, &ce) {
		return err
	}
	fmt.Fprintf(stderr, "closed: %d %s\n", int(ce.Code), ce.Reason)
	if ce.Code == websocket.StatusNormalClosure || ce.Code == websocket.StatusGoingAway {
		return nil
	}
	return ce
}
