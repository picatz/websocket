package websocket

import (
	"context"
	"fmt"
)

// watch arranges for ctx to abort the connection while an operation runs.
// If ctx is already done, it returns the context's cause and the operation
// must not start. Otherwise the caller passes the returned stop to ctxError
// when the operation ends. A context that can never be done, such as
// context.Background, costs nothing: stop is nil.
func (c *Conn) watch(ctx context.Context) (stop func() bool, err error) {
	if ctx.Done() == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	return context.AfterFunc(ctx, c.cancelIO), nil
}

// ctxError unregisters stop and returns err, wrapped with ctx's cause when
// ctx ended the operation, so the result matches both.
func ctxError(ctx context.Context, stop func() bool, err error) error {
	if stop != nil && !stop() && err != nil {
		return fmt.Errorf("%w: %w", context.Cause(ctx), err)
	}
	return err
}

// cancelIO aborts the connection on behalf of an ended context. Like a read
// failure, it publishes terminal state first, so a later Close reports
// net.ErrClosed and concurrent operations observe a closed connection.
func (c *Conn) cancelIO() {
	c.closeMu.Lock()
	c.closed.Store(true)
	c.markDoneLocked()
	c.failed = true
	c.closeMu.Unlock()
	c.abort()
}
