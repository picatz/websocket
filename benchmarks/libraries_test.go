package benchmarks

import (
	"context"
	"io"
	"net"
	"net/http"

	coder "github.com/coder/websocket"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	gorilla "github.com/gorilla/websocket"
	picatz "github.com/picatz/websocket"
)

// readLimit is the incoming message limit configured for every library, so
// that no library rejects a benchmark payload. It matches
// picatz.DefaultMaxMessageSize.
const readLimit = 1 << 20

// library adapts one WebSocket implementation to the workloads. Each adapter
// uses the library's documented, idiomatic message API with compression off.
type library struct {
	name string
	// echo is a server handler that echoes binary messages until the
	// connection ends.
	echo http.HandlerFunc
	// dial opens a client connection to a ws:// URL.
	dial func(ctx context.Context, url string) (client, error)
}

// client is the client side of one connection.
type client interface {
	write(p []byte) error
	read() ([]byte, error)
	close() error
}

var libraries = []library{
	{
		name: "picatz",
		echo: func(w http.ResponseWriter, r *http.Request) {
			conn, err := picatz.Upgrade(w, r)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				op, p, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err := conn.WriteMessage(op, p); err != nil {
					return
				}
			}
		},
		dial: func(ctx context.Context, url string) (client, error) {
			conn, _, err := picatz.Dial(ctx, url)
			if err != nil {
				return nil, err
			}
			return picatzClient{conn}, nil
		},
	},
	{
		name: "coder",
		echo: func(w http.ResponseWriter, r *http.Request) {
			conn, err := coder.Accept(w, r, &coder.AcceptOptions{CompressionMode: coder.CompressionDisabled})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			conn.SetReadLimit(readLimit)
			ctx := context.Background()
			for {
				typ, p, err := conn.Read(ctx)
				if err != nil {
					return
				}
				if err := conn.Write(ctx, typ, p); err != nil {
					return
				}
			}
		},
		dial: func(ctx context.Context, url string) (client, error) {
			conn, _, err := coder.Dial(ctx, url, &coder.DialOptions{CompressionMode: coder.CompressionDisabled})
			if err != nil {
				return nil, err
			}
			conn.SetReadLimit(readLimit)
			return coderClient{conn}, nil
		},
	},
	{
		name: "gorilla",
		echo: func(w http.ResponseWriter, r *http.Request) {
			upgrader := gorilla.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetReadLimit(readLimit)
			for {
				typ, p, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err := conn.WriteMessage(typ, p); err != nil {
					return
				}
			}
		},
		dial: func(ctx context.Context, url string) (client, error) {
			conn, _, err := gorilla.DefaultDialer.DialContext(ctx, url, nil)
			if err != nil {
				return nil, err
			}
			conn.SetReadLimit(readLimit)
			return gorillaClient{conn}, nil
		},
	},
	{
		name: "gobwas",
		echo: func(w http.ResponseWriter, r *http.Request) {
			conn, _, _, err := ws.UpgradeHTTP(r, w)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				p, op, err := wsutil.ReadClientData(conn)
				if err != nil {
					return
				}
				if err := wsutil.WriteServerMessage(conn, op, p); err != nil {
					return
				}
			}
		},
		dial: func(ctx context.Context, url string) (client, error) {
			conn, br, _, err := ws.Dial(ctx, url)
			if err != nil {
				return nil, err
			}
			c := gobwasClient{conn: conn, r: conn}
			if br != nil {
				// The handshake read ahead; drain those bytes first.
				c.r = struct {
					io.Reader
					io.Writer
				}{br, conn}
			}
			return &c, nil
		},
	},
}

type picatzClient struct{ c *picatz.Conn }

func (c picatzClient) write(p []byte) error { return c.c.WriteMessage(picatz.BinaryMessage, p) }
func (c picatzClient) close() error         { return c.c.Close() }
func (c picatzClient) read() ([]byte, error) {
	_, p, err := c.c.ReadMessage()
	return p, err
}

type coderClient struct{ c *coder.Conn }

func (c coderClient) write(p []byte) error {
	return c.c.Write(context.Background(), coder.MessageBinary, p)
}
func (c coderClient) close() error { return c.c.CloseNow() }
func (c coderClient) read() ([]byte, error) {
	_, p, err := c.c.Read(context.Background())
	return p, err
}

type gorillaClient struct{ c *gorilla.Conn }

func (c gorillaClient) write(p []byte) error { return c.c.WriteMessage(gorilla.BinaryMessage, p) }
func (c gorillaClient) close() error         { return c.c.Close() }
func (c gorillaClient) read() ([]byte, error) {
	_, p, err := c.c.ReadMessage()
	return p, err
}

type gobwasClient struct {
	conn net.Conn
	// r reads from conn, after any bytes buffered during the handshake.
	r io.ReadWriter
}

func (c *gobwasClient) write(p []byte) error { return wsutil.WriteClientBinary(c.conn, p) }
func (c *gobwasClient) close() error         { return c.conn.Close() }
func (c *gobwasClient) read() ([]byte, error) {
	return wsutil.ReadServerBinary(c.r)
}
