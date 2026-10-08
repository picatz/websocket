package websocket

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This local-only certificate is independent of the production TLS policy.
var failureTLSCertificate = sync.OnceValues(func() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "failure.test"}, DNSNames: []string{"failure.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, err
})

type failureTLSTraceConn struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
	reads     chan struct{}
	writes    chan struct{}
	closes    atomic.Int32
}

func (c *failureTLSTraceConn) Read(p []byte) (int, error) {
	select {
	case c.reads <- struct{}{}:
	default:
	}
	return c.Conn.Read(p)
}
func (c *failureTLSTraceConn) Write(p []byte) (int, error) {
	select {
	case c.writes <- struct{}{}:
	default:
	}
	return c.Conn.Write(p)
}
func (c *failureTLSTraceConn) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, d)
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(d)
}
func (c *failureTLSTraceConn) Close() error { c.closes.Add(1); return c.Conn.Close() }
func (c *failureTLSTraceConn) deadlineCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deadlines)
}
func failureTLSDrain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// Both TLS roles complete the handshake before exposing an unbuffered transport.
// A peer that stops reading therefore really blocks close_notify, unlike a TCP
// peer whose receive buffer can absorb the alert without processing it.
func failureTLSPair(t *testing.T, server bool) (*tls.Conn, *tls.Conn, *failureTLSTraceConn, net.Conn) {
	t.Helper()
	cert, err := failureTLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	a, b := net.Pipe()
	// Bound direct test-peer reads/writes as well as the library operation.
	// Set these below the trace wrapper so failure-path setter assertions
	// measure only changes made after the established TLS handshake.
	deadline := time.Now().Add(3 * time.Second)
	if err := a.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := b.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	raw := &failureTLSTraceConn{Conn: a, reads: make(chan struct{}, 32), writes: make(chan struct{}, 32)}
	serverConfig := &tls.Config{Certificates: []tls.Certificate{cert}, SessionTicketsDisabled: true}
	clientConfig := &tls.Config{RootCAs: roots, ServerName: "failure.test"}
	var target, peer *tls.Conn
	if server {
		target = tls.Server(raw, serverConfig)
		peer = tls.Client(b, clientConfig)
	} else {
		target = tls.Client(raw, clientConfig)
		peer = tls.Server(b, serverConfig)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- peer.HandshakeContext(ctx) }()
	if err := target.HandshakeContext(ctx); err != nil {
		a.Close()
		b.Close()
		<-done
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	failureTLSDrain(raw.reads)
	failureTLSDrain(raw.writes)
	return target, peer, raw, b
}

func failureTLSWaitSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
func failureTLSWaitError(t *testing.T, ch <-chan error, label string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return nil
	}
}
func failureTLSWire(server bool) []byte {
	if server {
		return []byte{0x81, 0x81, 0x11, 0x22, 0x33, 0x44, 0xff ^ 0x11}
	}
	return []byte{0x81, 1, 0xff}
}
func failureTLSReadClose(r io.Reader, server bool, want uint16) error {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return err
	}
	if h[0] != 0x88 || h[1]&127 != 2 || (h[1]&128 != 0) == server {
		return fmt.Errorf("unexpected Close header %x", h)
	}
	var key [4]byte
	if !server {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return err
		}
	}
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return err
	}
	if !server {
		p[0] ^= key[0]
		p[1] ^= key[1]
	}
	if got := binary.BigEndian.Uint16(p[:]); got != want {
		return fmt.Errorf("Close code %d, want %d", got, want)
	}
	return nil
}

func TestFailureTLSNewConnAbortsWithoutCloseNotify(t *testing.T) {
	for _, server := range []bool{false, true} {
		t.Run(fmt.Sprint(server), func(t *testing.T) {
			target, peer, raw, peerRaw := failureTLSPair(t, server)
			c := NewConn(target, server, nil)
			if c.abortConn != raw {
				t.Fatal("NewConn did not retain the concrete TLS underlying transport")
			}
			deadline := time.Now().Add(10 * time.Second)
			if err := raw.SetWriteDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			setters := raw.deadlineCount()
			var wg sync.WaitGroup
			t.Cleanup(func() { raw.Close(); peerRaw.Close(); wg.Wait() })
			peerDone := make(chan error, 1)
			wg.Go(func() {
				if _, err := peer.Write(failureTLSWire(server)); err != nil {
					peerDone <- err
					return
				}
				peerDone <- failureTLSReadClose(peer, server, 1007)
			})
			readDone := make(chan error, 1)
			start := time.Now()
			wg.Go(func() { _, _, err := c.ReadMessage(); readDone <- err })
			if err := failureTLSWaitError(t, peerDone, "error Close"); err != nil {
				t.Fatal(err)
			}
			// The peer performs no further TLS read, so tls.Conn.Close would block here.
			if err := failureTLSWaitError(t, readDone, "failure abort"); err != ErrInvalidFrame {
				t.Fatalf("error=%v", err)
			}
			if time.Since(start) > 750*time.Millisecond {
				t.Fatal("failure cleanup waited after the WebSocket notification")
			}
			if raw.deadlineCount() != setters {
				t.Fatal("failure cleanup changed the underlying write deadline")
			}
			if raw.closes.Load() != 1 {
				t.Fatalf("abort count=%d", raw.closes.Load())
			}
			if _, _, err := c.ReadMessage(); err != io.ErrClosedPipe {
				t.Fatal(err)
			}
		})
	}
}

func TestFailureTLSConcurrentCloseCancelsNotification(t *testing.T) {
	target, peer, raw, peerRaw := failureTLSPair(t, false)
	c := NewConn(target, false, nil)
	var wg sync.WaitGroup
	t.Cleanup(func() { raw.Close(); peerRaw.Close(); wg.Wait() })
	peerDone := make(chan error, 1)
	wg.Go(func() { _, err := peer.Write(failureTLSWire(false)); peerDone <- err })
	readDone := make(chan error, 1)
	wg.Go(func() { _, _, err := c.ReadMessage(); readDone <- err })
	if err := failureTLSWaitError(t, peerDone, "invalid frame write"); err != nil {
		t.Fatal(err)
	}
	failureTLSWaitSignal(t, raw.writes, "blocked TLS error notification")
	start := time.Now()
	closeDone := make(chan error, 1)
	wg.Go(func() { closeDone <- c.Close() })
	if err := failureTLSWaitError(t, closeDone, "concurrent Close"); err != ErrAlreadyClosed {
		t.Fatal(err)
	}
	if err := failureTLSWaitError(t, readDone, "canceled read failure"); err != ErrInvalidFrame {
		t.Fatal(err)
	}
	if time.Since(start) > 750*time.Millisecond {
		t.Fatal("concurrent Close waited for the notification watchdog")
	}
	if raw.deadlineCount() != 0 || raw.closes.Load() != 1 {
		t.Fatalf("deadlines=%d closes=%d", raw.deadlineCount(), raw.closes.Load())
	}
}

func TestFailureTLSNormalCloseFirstStillAllowsAbort(t *testing.T) {
	target, peer, raw, peerRaw := failureTLSPair(t, false)
	c := NewConn(target, false, nil)
	var wg sync.WaitGroup
	t.Cleanup(func() { raw.Close(); peerRaw.Close(); wg.Wait() })
	readDone := make(chan error, 1)
	wg.Go(func() { _, _, err := c.ReadMessage(); readDone <- err })
	failureTLSWaitSignal(t, raw.reads, "already-running TLS read")
	closeDone := make(chan error, 1)
	wg.Go(func() { closeDone <- c.Close() })
	if err := failureTLSReadClose(peer, false, 1000); err != nil {
		t.Fatal(err)
	}
	failureTLSWaitSignal(t, raw.writes, "normal WebSocket Close write")
	failureTLSWaitSignal(t, raw.writes, "blocked TLS close_notify write")
	// The normal closer is now inside TLS Close. Inject the failure into the
	// preexisting read; it must not queue behind graceful-close ownership.
	start := time.Now()
	if _, err := peer.Write(failureTLSWire(false)); err != nil {
		t.Fatal(err)
	}
	if err := failureTLSWaitError(t, readDone, "failure during graceful TLS Close"); err != ErrInvalidFrame {
		t.Fatal(err)
	}
	_ = failureTLSWaitError(t, closeDone, "interrupted graceful TLS Close")
	if time.Since(start) > 750*time.Millisecond {
		t.Fatal("abort waited behind graceful TLS Close")
	}
	// Graceful TLS Close and independent abort may both call underlying Close.
	if raw.closes.Load() < 1 {
		t.Fatal("underlying transport was not aborted")
	}
}

func TestFailureTLSUpgradeRetainsAbortTransport(t *testing.T) {
	target, peer, raw, peerRaw := failureTLSPair(t, true)
	var wg sync.WaitGroup
	t.Cleanup(func() { raw.Close(); peerRaw.Close(); wg.Wait() })
	peerDone := make(chan error, 1)
	wg.Go(func() {
		br := bufio.NewReader(peer)
		resp, err := http.ReadResponse(br, upgradeRequest())
		if err != nil {
			peerDone <- err
			return
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			peerDone <- fmt.Errorf("upgrade status=%d", resp.StatusCode)
			return
		}
		if _, err := peer.Write(failureTLSWire(true)); err != nil {
			peerDone <- err
			return
		}
		peerDone <- failureTLSReadClose(br, true, 1007)
	})
	rw := bufio.NewReadWriter(bufio.NewReader(target), bufio.NewWriter(target))
	c, err := Upgrade(&hijackResponse{httptest.NewRecorder(), target, rw}, upgradeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if c.conn != target || c.abortConn != raw {
		t.Fatal("Upgrade lost the concrete TLS abort target")
	}
	setters := raw.deadlineCount()
	readDone := make(chan error, 1)
	start := time.Now()
	wg.Go(func() { _, _, err := c.ReadMessage(); readDone <- err })
	if err := failureTLSWaitError(t, peerDone, "Upgrade error Close"); err != nil {
		t.Fatal(err)
	}
	if err := failureTLSWaitError(t, readDone, "Upgrade failure abort"); err != ErrInvalidFrame {
		t.Fatal(err)
	}
	if time.Since(start) > 750*time.Millisecond || raw.deadlineCount() != setters {
		t.Fatal("Upgrade failure cleanup waited or altered deadlines")
	}
}

func TestFailureTLSDialRetainsRawAbortTransport(t *testing.T) {
	peerDone := make(chan error, 1)
	peerFinished := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(peerFinished)
		raw, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			peerDone <- err
			return
		}
		defer raw.Close()
		if err = raw.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			peerDone <- err
			return
		}
		if _, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key"))); err != nil {
			peerDone <- err
			return
		}
		if _, err = rw.Write(failureTLSWire(false)); err != nil {
			peerDone <- err
			return
		}
		if err = rw.Flush(); err != nil {
			peerDone <- err
			return
		}
		peerDone <- failureTLSReadClose(rw, false, 1007)
	}))
	defer srv.Close()
	config := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	c, _, err := Dial(t.Context(), "wss"+srv.URL[5:], WithTLSConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.abort()
		failureTLSWaitSignal(t, peerFinished, "hijacked TLS peer cleanup")
	})
	tlsConn, ok := c.conn.(*tls.Conn)
	if !ok || c.abortConn != tlsConn.NetConn() {
		t.Fatal("Dial did not preserve its raw TLS transport")
	}
	if _, _, err := c.ReadMessage(); err != ErrInvalidFrame {
		t.Fatal(err)
	}
	if err := failureTLSWaitError(t, peerDone, "Dial error Close"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.abortConn.Write([]byte{0}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("raw transport remains writable: %v", err)
	}
	if _, _, err := c.ReadMessage(); err != io.ErrClosedPipe {
		t.Fatal(err)
	}
}
