package websocket

import (
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// upgradeOriginAllowed validates syntax before handing policy to the caller.
// Iterate the map because Header.Get alone misses noncanonical or repeated keys.
func upgradeOriginAllowed(r *http.Request, check func(*http.Request, string) bool) bool {
	origin, present := "", false
	for name, values := range r.Header {
		if !strings.EqualFold(name, "Origin") {
			continue
		}
		if present || len(values) != 1 {
			return false
		}
		present = true
		var ok bool
		origin, ok = canonicalOrigin(strings.Trim(values[0], " \t"))
		if !ok {
			return false
		}
	}
	if check != nil {
		return check(r, origin)
	}
	if !present {
		return true
	}
	if origin == "null" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	expected, ok := canonicalOrigin(scheme + "://" + r.Host)
	return ok && origin == expected
}

// canonicalOrigin accepts one serialized tuple, not a URL with optional path,
// userinfo, query, or fragment. The deliberately conservative host grammar is
// ASCII RFC 3986 reg-name characters except percent escapes and commas, or a
// bracketed IPv6 address. Commas conflict with our single-origin policy. It neither
// resolves names nor applies IDNA, DNS suffix, or IPv4 alias normalization.
func canonicalOrigin(value string) (string, bool) {
	if value == "null" {
		return value, true
	}
	scheme, authority, ok := strings.Cut(value, "://")
	if !ok || scheme == "" || authority == "" {
		return "", false
	}
	for i := range len(scheme) {
		c := scheme[i]
		if !asciiLetter(c) && (i == 0 || (c < '0' || c > '9') && c != '+' && c != '-' && c != '.') {
			return "", false
		}
	}
	scheme = strings.ToLower(scheme)
	var host, port string
	var hasPort bool
	if authority[0] == '[' {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", false
		}
		addr, err := netip.ParseAddr(authority[1:end])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", false
		}
		host = "[" + addr.String() + "]"
		if addr.Is4In6() {
			// Browser origin serialization keeps mapped IPv6 in hexadecimal.
			b := addr.As16()
			host = "[::ffff:" + strconv.FormatUint(uint64(b[12])<<8|uint64(b[13]), 16) +
				":" + strconv.FormatUint(uint64(b[14])<<8|uint64(b[15]), 16) + "]"
		}
		if rest := authority[end+1:]; rest != "" {
			if rest[0] != ':' {
				return "", false
			}
			port, hasPort = rest[1:], true
		}
	} else {
		host, port, hasPort = strings.Cut(authority, ":")
		if host == "" {
			return "", false
		}
		for i := range len(host) {
			c := host[i]
			if !asciiLetter(c) && (c < '0' || c > '9') && !strings.ContainsRune("-._~!$&'()*+;=", rune(c)) {
				return "", false
			}
		}
		host = strings.ToLower(host)
	}
	if hasPort {
		if port == "" {
			return "", false
		}
		for i := range len(port) {
			if port[i] < '0' || port[i] > '9' {
				return "", false
			}
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", false
		}
		if !(scheme == "http" && n == 80 || scheme == "https" && n == 443) {
			host += ":" + strconv.FormatUint(n, 10)
		}
	}
	return scheme + "://" + host, true
}

func asciiLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
