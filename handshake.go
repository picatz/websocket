package websocket

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// validateHandshakeHeaders checks caller fields before acquiring a transport.
// Field names use the HTTP token alphabet; values follow net/http's allowance
// for HTAB, visible ASCII, and obs-text, excluding every other control byte. Do
// not include field values in errors: they may contain credentials or cookies.
func validateHandshakeHeaders(header http.Header, response bool) error {
	for name, values := range header {
		if !isHTTPToken(name) {
			return fmt.Errorf("%w: invalid field name %q", errInvalidHandshakeHeader, name)
		}
		reserved := false
		switch strings.ToLower(name) {
		case "upgrade", "connection", "sec-websocket-extensions", "content-length", "transfer-encoding":
			reserved = true
		case "host", "sec-websocket-key", "sec-websocket-version":
			reserved = !response
		case "sec-websocket-accept":
			reserved = response
		}
		if reserved {
			return fmt.Errorf("%w: reserved field %q", errInvalidHandshakeHeader, name)
		}
		for _, value := range values {
			for i := 0; i < len(value); i++ {
				c := value[i]
				if (c < ' ' && c != '\t') || c == 0x7f {
					return fmt.Errorf("%w: invalid value for field %q", errInvalidHandshakeHeader, name)
				}
			}
		}
	}
	return nil
}

// subprotocolValues includes noncanonical map keys in caller-provided headers.
// A nil/empty slice emits no field and is equivalent to an absent header.
func subprotocolValues(header http.Header) []string {
	var values []string
	for name, fields := range header {
		if strings.EqualFold(name, "Sec-WebSocket-Protocol") {
			values = append(values, fields...)
		}
	}
	return values
}

func isHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

// RFC 6455 sections 4.1 and 4.3 require unique, case-sensitive token offers.
// The HTTP 1#token list rule permits empty list elements, but at least one
// nonempty token is required when the header is present.
func parseSubprotocolOffers(header http.Header) (map[string]struct{}, error) {
	values := subprotocolValues(header)
	var offered map[string]struct{}
	for _, value := range values {
		for item := range strings.SplitSeq(value, ",") {
			token := strings.Trim(item, " \t")
			if token == "" {
				continue
			}
			if !isHTTPToken(token) {
				return nil, errInvalidSubprotocol
			}
			if _, duplicate := offered[token]; duplicate {
				return nil, errInvalidSubprotocol
			}
			if offered == nil {
				offered = make(map[string]struct{}, 1)
			}
			offered[token] = struct{}{}
		}
	}
	if len(values) != 0 && len(offered) == 0 {
		return nil, errInvalidSubprotocol
	}
	return offered, nil
}

// A response may omit selection, or contain exactly one field with one offered
// token. In particular, repeated response fields and comma lists are forbidden.
// It returns the selected token, or "" when the header is absent.
func validateSubprotocolSelection(header http.Header, offered map[string]struct{}) (string, error) {
	values := subprotocolValues(header)
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", errInvalidSubprotocol
	}
	selected := strings.Trim(values[0], " \t")
	if !isHTTPToken(selected) {
		return "", errInvalidSubprotocol
	}
	if _, ok := offered[selected]; !ok {
		return "", errInvalidSubprotocol
	}
	return selected, nil
}

// computeAcceptKey computes the Sec-WebSocket-Accept value.
func computeAcceptKey(challengeKey string) string {
	accept := acceptKey([]byte(challengeKey))
	return string(accept[:])
}

// acceptGUID is the GUID RFC 6455 section 1.3 appends to the client's key.
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// acceptKey returns the Sec-WebSocket-Accept value for a 24-byte challenge
// key without allocating; longer keys, which a server rejects first, cost
// one allocation.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-1.3
func acceptKey(challengeKey []byte) [28]byte {
	var buf [24 + len(acceptGUID)]byte
	sum := sha1.Sum(append(append(buf[:0], challengeKey...), acceptGUID...))
	var accept [28]byte
	base64.StdEncoding.Encode(accept[:], sum[:])
	return accept
}

// validChallengeKey reports whether key is a Sec-WebSocket-Key value: the
// base64 encoding of 16 bytes, which is always 24 characters long.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-4.2.1
func validChallengeKey(key string) bool {
	if len(key) != 24 {
		return false
	}
	var nonce [18]byte
	n, err := base64.StdEncoding.Decode(nonce[:], []byte(key))
	return err == nil && n == 16
}

// generateKey generates a Sec-WebSocket-Key value: a random 16-byte nonce,
// base64-encoded.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-4.1
func generateKey() [24]byte {
	var nonce [16]byte
	rand.Read(nonce[:]) // never fails; see crypto/rand.Read
	var key [24]byte
	base64.StdEncoding.Encode(key[:], nonce[:])
	return key
}

// headerContains checks if a header contains a specific value,
// based on case-insensitive comparison to allow for case variations,
// and handling of multiple values separated by commas.
func headerContains(h http.Header, name string, value string) bool {
	for _, s := range h[name] {
		for v := range strings.SplitSeq(s, ",") {
			if strings.EqualFold(strings.TrimSpace(v), value) {
				return true
			}
		}
	}
	return false
}
