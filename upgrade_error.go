package websocket

import (
	"errors"
	"fmt"
	"net/http"
)

// upgradeErrorStatus returns the HTTP status Upgrade writes for err, or 0 when
// it writes nothing:
//
//   - ErrOriginNotAllowed: 403 Forbidden.
//   - ErrInvalidMethod: 405 Method Not Allowed, with Allow: GET.
//   - ErrUnsupportedVersion: 426 Upgrade Required, with
//     Sec-WebSocket-Version: 13 (RFC 6455 section 4.4).
//   - ErrNotHijacker, and an invalid Upgrader (ErrInvalidConfig, which
//     ErrInvalidHandshakeHeader also matches): 500 Internal Server Error.
//   - ErrHandshakeFailed: 0. The transport may already be hijacked.
//   - Any other error, which is a malformed client handshake: 400 Bad Request.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-4.2.1
func upgradeErrorStatus(err error) int {
	switch {
	case err == nil, errors.Is(err, ErrHandshakeFailed):
		return 0
	case errors.Is(err, ErrOriginNotAllowed):
		return http.StatusForbidden
	case errors.Is(err, ErrInvalidMethod):
		return http.StatusMethodNotAllowed
	case errors.Is(err, ErrUnsupportedVersion):
		return http.StatusUpgradeRequired
	case errors.Is(err, ErrNotHijacker), errors.Is(err, ErrInvalidConfig):
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

// writeUpgradeError writes the response upgradeErrorStatus selects and returns
// err, wrapped with ErrBadHandshake when the client's request was at fault.
// It sets the header fields the status requires, then calls write, or
// defaultUpgradeError when write is nil.
func writeUpgradeError(w http.ResponseWriter, r *http.Request, err error, write func(http.ResponseWriter, *http.Request, int, error)) error {
	status := upgradeErrorStatus(err)
	if status == 0 {
		return err
	}
	if status != http.StatusForbidden && status < http.StatusInternalServerError && !errors.Is(err, ErrBadHandshake) {
		err = fmt.Errorf("%w: %w", ErrBadHandshake, err)
	}
	switch status {
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", http.MethodGet)
	case http.StatusUpgradeRequired:
		w.Header().Set("Sec-WebSocket-Version", "13")
	}
	if write == nil {
		write = defaultUpgradeError
	}
	write(w, r, status, err)
	return err
}

// defaultUpgradeError writes a fixed plain-text body. Error details stay in
// the returned error for the server's logs.
func defaultUpgradeError(w http.ResponseWriter, _ *http.Request, status int, _ error) {
	http.Error(w, "WebSocket: "+http.StatusText(status), status)
}
