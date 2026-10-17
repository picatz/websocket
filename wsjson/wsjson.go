// Package wsjson reads and writes JSON values as WebSocket text messages.
//
// It is a separate package so the websocket core keeps no encoding
// dependencies beyond what a connection needs. Each value is one message, so
// the connection's incoming size limit bounds what Read decodes.
package wsjson

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/picatz/websocket"
)

// ErrNotText reports a binary message where Read expected JSON text.
var ErrNotText = errors.New("wsjson: message is not text")

// Read reads the next message from c and decodes it into v with
// json.Unmarshal. ctx bounds the read as for Conn.ReadMessage, so ctx ending
// mid-read aborts the connection. A binary message returns an error matching ErrNotText; a
// message that is not valid JSON for v returns the decoding error. Neither
// closes the connection.
func Read(ctx context.Context, c *websocket.Conn, v any) error {
	typ, data, err := c.ReadMessage(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.TextMessage {
		return fmt.Errorf("%w: got %v", ErrNotText, typ)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("wsjson: decode: %w", err)
	}
	return nil
}

// Write encodes v with json.Marshal and sends it to c as one text message. ctx
// bounds the write as for Conn.WriteMessage, so ctx ending mid-write aborts
// the connection. An
// encoding error is returned before any I/O.
func Write(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("wsjson: encode: %w", err)
	}
	return c.WriteMessage(ctx, websocket.TextMessage, data)
}
