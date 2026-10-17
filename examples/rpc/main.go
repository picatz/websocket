// Command rpc is a [JSON-RPC 2.0] server over WebSocket.
//
// Each text message from a client holds one request object, and each reply
// one response object:
//
//	→ {"jsonrpc":"2.0","id":1,"method":"sum","params":[1,2,3]}
//	← {"jsonrpc":"2.0","id":1,"result":6}
//
// Requests run concurrently, up to maxInFlight per connection, so responses
// can arrive in a different order than their requests; clients match them by
// id. A request without an id is a notification and gets no response. Batch
// requests are not supported and are answered with an Invalid Request error.
//
// The read loop is the only reader and stops reading while maxInFlight calls
// are running, which pushes back on a client that sends faster than the server
// answers. Conn serializes the concurrent writes of the responses.
//
// Calls still running when the client closes the connection are abandoned:
// once the client's Close frame arrives, the server can send nothing more.
//
// Run it, then type requests into the client example:
//
//	go run ./examples/rpc
//	go run ./examples/client ws://localhost:8080/rpc
//	{"jsonrpc":"2.0","id":1,"method":"sum","params":[1,2,3]}
//	{"jsonrpc":"2.0","id":1,"result":6}
//
// [JSON-RPC 2.0]: https://www.jsonrpc.org/specification
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/picatz/websocket"
	"github.com/picatz/websocket/wsjson"
)

const (
	// maxInFlight bounds the calls one connection may have running.
	maxInFlight = 8
	// maxMessage bounds each incoming request.
	maxMessage = 64 << 10
	// writeWait bounds each response.
	writeWait = 5 * time.Second
)

// request is a JSON-RPC request or notification.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // nil for a notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is a JSON-RPC response; exactly one of Result and Error is set.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object. Methods return one to choose the code
// the client sees; any other error becomes an Internal error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Error codes defined by the specification.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// A method handles one call. ctx ends when the connection does.
type method func(ctx context.Context, params json.RawMessage) (any, error)

var methods = map[string]method{
	"sum":   sum,
	"sleep": sleep,
}

// sum returns the sum of an array of numbers.
func sum(_ context.Context, params json.RawMessage) (any, error) {
	var nums []float64
	if err := json.Unmarshal(params, &nums); err != nil {
		return nil, &Error{codeInvalidParams, "params must be an array of numbers"}
	}
	var total float64
	for _, n := range nums {
		total += n
	}
	return total, nil
}

// sleep waits for params[0] milliseconds, at most one second, and returns
// them. It shows responses overtaking each other.
func sleep(ctx context.Context, params json.RawMessage) (any, error) {
	var ms []int
	if err := json.Unmarshal(params, &ms); err != nil || len(ms) != 1 || ms[0] < 0 || ms[0] > 1000 {
		return nil, &Error{codeInvalidParams, "params must be [milliseconds], from 0 to 1000"}
	}
	select {
	case <-time.After(time.Duration(ms[0]) * time.Millisecond):
		return ms[0], nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// handle answers one connection's requests until it closes.
func handle(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{MaxMessageSize: maxMessage}
	conn, err := upgrader.Upgrade(w, r)
	if err != nil {
		return // Upgrade has already written the HTTP error response.
	}
	defer conn.Close()

	// Deferred calls run in reverse: cancel running calls, wait for them,
	// then close the connection.
	var calls sync.WaitGroup
	defer calls.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reply := func(resp response) {
		resp.JSONRPC = "2.0"
		writeCtx, cancel := context.WithTimeout(ctx, writeWait)
		defer cancel()
		if err := wsjson.Write(writeCtx, conn, resp); err != nil {
			conn.Close() // Ends the read loop.
		}
	}

	inFlight := make(chan struct{}, maxInFlight)
	for {
		typ, data, err := conn.ReadMessage(r.Context())
		if err != nil {
			return
		}
		var req request
		switch {
		case typ != websocket.TextMessage:
			reply(response{ID: null, Error: &Error{codeInvalidRequest, "requests must be text messages"}})
			continue
		case json.Unmarshal(data, &req) != nil:
			if !json.Valid(data) {
				reply(response{ID: null, Error: &Error{codeParseError, "invalid JSON"}})
			} else {
				reply(response{ID: null, Error: &Error{codeInvalidRequest, "not a request object"}})
			}
			continue
		case req.JSONRPC != "2.0" || req.Method == "":
			reply(response{ID: idOrNull(req.ID), Error: &Error{codeInvalidRequest, `want "jsonrpc":"2.0" and a method`}})
			continue
		}

		inFlight <- struct{}{} // Stop reading while maxInFlight calls run.
		calls.Go(func() {
			defer func() { <-inFlight }()
			resp := call(ctx, req)
			if req.ID != nil {
				reply(resp)
			}
		})
	}
}

// null is the id of a response to a request whose id could not be read.
var null = json.RawMessage("null")

func idOrNull(id json.RawMessage) json.RawMessage {
	if id == nil {
		return null
	}
	return id
}

// call runs req's method and builds its response.
func call(ctx context.Context, req request) response {
	resp := response{ID: req.ID}
	m, ok := methods[req.Method]
	if !ok {
		resp.Error = &Error{codeMethodNotFound, "method not found: " + req.Method}
		return resp
	}
	result, err := m(ctx, req.Params)
	if err == nil {
		resp.Result, err = json.Marshal(result)
	}
	if err != nil {
		if !errors.As(err, &resp.Error) {
			resp.Error = &Error{codeInternalError, "internal error"}
			log.Printf("%s: %v", req.Method, err)
		}
		resp.Result = nil
	}
	return resp
}

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	http.HandleFunc("/rpc", handle)
	log.Printf("JSON-RPC server listening on ws://%s/rpc", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
