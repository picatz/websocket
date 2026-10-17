package websocket

// Unexported errors that the external tests assert on. They are details of
// the exported errors, so callers match those instead.
var (
	ErrWriteFailedForTest            = errWriteFailed
	ErrProxyForTest                  = errProxy
	ErrResponseHeaderTooLargeForTest = errResponseHeaderTooLarge
)
