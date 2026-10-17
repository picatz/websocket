package websocket

import (
	"crypto/tls"
	"net"
)

// LocalAddr returns the local network address of the underlying transport.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address of the underlying transport.
// Behind a proxy it is the proxy's address, as with net/http.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// ConnectionState returns the TLS state of a wss connection from Dial, or of a
// connection Upgrade took over from an HTTPS server. It reports false when the
// transport is not a *tls.Conn, including a NewConn transport that wraps TLS
// in another type.
func (c *Conn) ConnectionState() (tls.ConnectionState, bool) {
	if tlsConn, ok := c.conn.(*tls.Conn); ok {
		return tlsConn.ConnectionState(), true
	}
	return tls.ConnectionState{}, false
}
