package websocket

import "net"

// LocalAddr returns the local network address of the underlying transport.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address of the underlying transport.
// Behind a proxy it is the proxy's address, as with net/http.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}
