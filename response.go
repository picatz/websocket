package websocket

import (
	"bufio"
	"errors"
	"io"
	"net/http"
)

// responseHeadReader bounds parser input without allocating the entire budget.
// A negative remaining count disables the bound after a successful parse.
type responseHeadReader struct {
	r         io.Reader
	remaining int64
	exceeded  bool
	readErr   error
}

func (r *responseHeadReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		// An error returned with the final allowed bytes is still the cause
		// of a truncated response, rather than an attempted over-budget read.
		if r.readErr != nil {
			return 0, r.readErr
		}
		r.exceeded = true
		return 0, ErrResponseHeaderTooLarge
	}
	if r.remaining > 0 && int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.readErr = err
	if r.remaining > 0 {
		r.remaining -= int64(n)
	}
	return n, err
}

func readHandshakeResponse(src io.Reader, maxBytes int64) (*http.Response, *bufio.Reader, error) {
	if maxBytes <= 0 {
		maxBytes = -1
	}
	r := &responseHeadReader{r: src, remaining: maxBytes}
	br := bufio.NewReader(r)
	resp, err := http.ReadResponse(br, nil)
	// ReadLine can discard the reader error while returning a partial line,
	// including when the budget splits CR/LF. Merely reaching zero is fine:
	// reject only when parsing actually tried to read beyond the budget.
	if r.exceeded {
		return nil, br, ErrResponseHeaderTooLarge
	}
	if err != nil {
		if r.readErr != nil && !errors.Is(err, r.readErr) {
			err = errors.Join(err, r.readErr)
		}
		return nil, br, err
	}
	// Preserve this same buffer for both response bodies and upgraded frames.
	// Prefetched bytes after the head do not consume a lasting frame/body cap.
	r.remaining = -1
	return resp, br, nil
}
