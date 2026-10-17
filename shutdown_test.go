package websocket_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// shutdownPair dials a server running handler and returns the client.
func shutdownPair(t *testing.T, handler func(*websocket.Conn)) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		handler(c)
	}))
	t.Cleanup(srv.Close)
	c, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func shutdownContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestShutdownCompletesHandshake(t *testing.T) {
	got := make(chan error, 1)
	c := shutdownPair(t, func(s *websocket.Conn) {
		_, _, err := s.ReadMessage()
		got <- err
	})
	if err := c.Shutdown(shutdownContext(t), websocket.StatusGoingAway, "restarting"); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	var ce *websocket.CloseError
	if err := <-got; !errors.As(err, &ce) || ce.Code != websocket.StatusGoingAway || ce.Reason != "restarting" {
		t.Fatalf("server read: err = %v, want StatusGoingAway \"restarting\"", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after Shutdown: err = %v, want net.ErrClosed", err)
	}
	if err := c.Shutdown(shutdownContext(t), websocket.StatusNormalClosure, ""); !errors.Is(err, websocket.ErrAlreadyClosed) {
		t.Fatalf("second Shutdown: err = %v, want ErrAlreadyClosed", err)
	}
}

// With a read loop running, Shutdown still ends the reading side: a data
// message the peer sends after receiving the Close frame is discarded, and the
// loop's ReadMessage returns the peer's CloseError or net.ErrClosed, never
// the message. The peer is scripted on a pipe so the order is exact.
func TestShutdownWithReadLoop(t *testing.T) {
	for range 50 {
		a, b := net.Pipe()
		peer := make(chan error, 1)
		go func() {
			defer b.Close()
			buf := make([]byte, 512)
			for {
				n, err := b.Read(buf)
				if err != nil {
					peer <- err
					return
				}
				if n > 0 && buf[0] == 0x88 { // the client's Close frame
					break
				}
			}
			late := []byte{0x81, 4, 'l', 'a', 't', 'e'}
			closeFrame := []byte{0x88, 2, 0x03, 0xe8} // StatusNormalClosure
			_, err := b.Write(append(late, closeFrame...))
			peer <- err
			_, _ = io.Copy(io.Discard, b)
		}()
		c := websocket.NewConn(a, websocket.ConnConfig{})

		reads := make(chan error, 1)
		go func() {
			for {
				_, data, err := c.ReadMessage()
				if err != nil {
					reads <- err
					return
				}
				reads <- fmt.Errorf("read loop got %q after Shutdown started", data)
				return
			}
		}()
		if err := c.Shutdown(shutdownContext(t), websocket.StatusNormalClosure, ""); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if err := <-peer; err != nil {
			t.Fatalf("peer: %v", err)
		}
		err := <-reads
		if websocket.CloseStatus(err) != websocket.StatusNormalClosure && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read loop: err = %v, want the peer's CloseError or net.ErrClosed", err)
		}
	}
}

// Without a read loop, Shutdown drains what the peer is still sending, so the
// peer's writes in flight succeed and it then reads the status code. Close
// would hang up on unread data, and the peer's writes would fail with a
// connection reset.
func TestShutdownDrainsUnreadMessages(t *testing.T) {
	writeErr, readErr := make(chan error, 1), make(chan error, 1)
	c := shutdownPair(t, func(s *websocket.Conn) {
		big := bytes.Repeat([]byte("x"), 64<<10)
		var err error
		for range 64 { // far more than the socket buffers hold
			if err = s.WriteMessage(websocket.BinaryMessage, big); err != nil {
				break
			}
		}
		writeErr <- err
		_, _, err = s.ReadMessage()
		readErr <- err
	})
	// Let the server fill the client's receive buffer before shutting down.
	time.Sleep(50 * time.Millisecond)
	if err := c.Shutdown(shutdownContext(t), websocket.StatusPolicyViolation, "too chatty"); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("server write during Shutdown: %v", err)
	}
	if err := <-readErr; websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("server read: err = %v, want StatusPolicyViolation", err)
	}
}

// A peer that never answers cannot hold Shutdown past its context.
func TestShutdownContextBoundsSilentPeer(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go func() { // read frames, never answer
		buf := make([]byte, 512)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	c := websocket.NewConn(a, websocket.ConnConfig{})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := c.Shutdown(ctx, websocket.StatusNormalClosure, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: err = %v, want context.DeadlineExceeded", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after timed-out Shutdown: err = %v, want net.ErrClosed", err)
	}
}

// A peer that drops the TCP connection instead of answering is an
// unfinished handshake, not success.
func TestShutdownPeerDropsConnection(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		buf := make([]byte, 512)
		_, _ = b.Read(buf) // the Close frame
		b.Close()
	}()
	c := websocket.NewConn(a, websocket.ConnConfig{})
	err := c.Shutdown(shutdownContext(t), websocket.StatusNormalClosure, "")
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: err = %v, want an unfinished-handshake error", err)
	}
}

func TestShutdownInvalidStatusLeavesConnOpen(t *testing.T) {
	echo := make(chan string, 1)
	c := shutdownPair(t, func(s *websocket.Conn) {
		_, data, _ := s.ReadMessage()
		echo <- string(data)
	})
	if err := c.Shutdown(shutdownContext(t), websocket.StatusAbnormalClosure, ""); !errors.Is(err, websocket.ErrInvalidCloseStatus) {
		t.Fatalf("Shutdown(1006): err = %v, want ErrInvalidCloseStatus", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte("still open")); err != nil {
		t.Fatalf("write after rejected Shutdown: %v", err)
	}
	if got := <-echo; got != "still open" {
		t.Fatalf("server got %q", got)
	}
}

// Shutdown ends a connection with the closing handshake, so the peer reads
// the status code even while it is still sending.
func ExampleConn_Shutdown() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		fmt.Println("server:", websocket.CloseStatus(err))
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Shutdown(ctx, websocket.StatusGoingAway, "server restarting"); err != nil {
		log.Print(err) // the connection is closed either way
	}
	fmt.Println("client: done")
	// Output:
	// server: StatusGoingAway
	// client: done
}

// A Close frame that cannot be written ends Shutdown with the write error and
// leaves the connection fully closed, so a later Close reports
// ErrAlreadyClosed instead of writing again.
func TestShutdownFailedWriteClosesConn(t *testing.T) {
	a, b := net.Pipe()
	b.Close()
	c := websocket.NewConn(a, websocket.ConnConfig{})
	if err := c.Shutdown(shutdownContext(t), websocket.StatusNormalClosure, ""); !errors.Is(err, websocket.ErrWriteFailed) {
		t.Fatalf("Shutdown: err = %v, want ErrWriteFailed", err)
	}
	if err := c.Close(); !errors.Is(err, websocket.ErrAlreadyClosed) {
		t.Fatalf("Close after failed Shutdown: err = %v, want ErrAlreadyClosed", err)
	}
}
