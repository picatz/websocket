//go:build js && wasm

package browser

// SetWriteHighWaterMark lowers the backpressure threshold for a test and
// returns a function that restores it.
func SetWriteHighWaterMark(n int) (restore func()) {
	old := writeHighWaterMark
	writeHighWaterMark = n
	return func() { writeHighWaterMark = old }
}

// BufferedAmount reports the host's unsent bytes for c.
func BufferedAmount(c *Conn) int { return c.ws.Get("bufferedAmount").Int() }
