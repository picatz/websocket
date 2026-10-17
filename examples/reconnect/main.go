// Command reconnect is a client that keeps a subscription alive across server
// restarts, deploys and network failures.
//
// It prints the text of every message it receives. When the connection ends
// for a reason worth retrying, it dials again after a random delay that grows
// exponentially with each failed attempt ("full jitter"), so that the
// thousands of clients one server restart disconnects do not all return in
// the same instant. A 429 or 503 response's Retry-After header sets a floor
// on that delay.
//
// The server numbers its messages: each one is a decimal sequence number, a
// space, and the text. On every dial the client sends the last number it saw
// in a Resume-After header, so the server can replay what was missed, and it
// drops any message it has already seen, so a replay that overlaps is
// harmless.
//
// A connection can also fail silently: a NAT or load balancer forgets it, and
// no packet ever says so. The client pings on an interval, and a Ping that
// goes unanswered ends the connection like any other failure.
//
// Not every failure is worth retrying. The client gives up when the handshake
// is refused with a 4xx status other than 408 and 429, when the server's
// certificate is rejected, when the server closes with StatusNormalClosure
// (the subscription is over) or with a status that says the client itself is
// at fault, such as StatusPolicyViolation, and when the server breaks the
// protocol. On an interrupt it closes with StatusGoingAway and exits.
//
//	go run ./examples/reconnect ws://localhost:8080/ws
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/picatz/websocket"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: reconnect [flags] ws://host/path")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := &client{
		url:          flag.Arg(0),
		backoff:      backoff{min: 500 * time.Millisecond, max: 30 * time.Second},
		pingInterval: 20 * time.Second,
		pongWait:     10 * time.Second,
		out:          os.Stdout,
		logf:         log.Printf,
	}
	if err := c.run(ctx); err != nil {
		log.Fatal(err)
	}
}

// client holds one subscription and the state that survives reconnects.
type client struct {
	url    string
	dialer websocket.Dialer

	backoff backoff
	// handshakeWait bounds each dial; zero means 10 seconds.
	handshakeWait time.Duration
	// pingInterval is the time between keepalive Pings, and pongWait how
	// long each waits for its Pong before the connection is given up.
	pingInterval, pongWait time.Duration

	// out receives the text of each new message, one per line.
	out  io.Writer
	logf func(format string, args ...any)

	// lastSeq is the highest sequence number received.
	lastSeq uint64
}

// run keeps the subscription connected until ctx ends, which returns nil, or
// until a failure that retrying cannot fix, which it returns.
func (c *client) run(ctx context.Context) error {
	attempt := 0
	for {
		conn, resp, err := c.dial(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var wait time.Duration
		if err != nil {
			if !retryableDial(resp, err) {
				return fmt.Errorf("dial %s: %w", c.url, err)
			}
			wait = retryAfter(resp)
			c.logf("dial failed: %v", err)
		} else {
			received, err := c.session(ctx, conn)
			if ctx.Err() != nil {
				return nil
			}
			if !retryableSession(err) {
				if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
					return nil
				}
				return fmt.Errorf("connection to %s: %w", c.url, err)
			}
			c.logf("connection lost: %v", err)
			// A connection that delivered messages was healthy, so the
			// next failure starts the backoff over.
			if received {
				attempt = 0
			}
		}

		wait = max(wait, c.backoff.delay(attempt))
		attempt++
		c.logf("reconnecting in %v", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// dial opens a connection that asks the server to resume after lastSeq.
func (c *client) dial(ctx context.Context) (*websocket.Conn, *http.Response, error) {
	wait := c.handshakeWait
	if wait == 0 {
		wait = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	// The Dialer is copied so each attempt sends its own header.
	d := c.dialer
	d.Header = d.Header.Clone()
	if d.Header == nil {
		d.Header = make(http.Header)
	}
	d.Header.Set("Resume-After", strconv.FormatUint(c.lastSeq, 10))
	return d.Dial(ctx, c.url)
}

// errPongTimeout ends a connection whose peer stopped answering Pings.
var errPongTimeout = errors.New("no pong from server")

// session reads conn until it fails or ctx ends, and reports whether any new
// message arrived. It always closes conn.
func (c *client) session(ctx context.Context, conn *websocket.Conn) (received bool, err error) {
	defer conn.Close()
	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// The keepalive and the caller's ctx end the connection by closing it,
	// which interrupts the read loop below.
	keepalive := make(chan struct{})
	go func() {
		defer close(keepalive)
		ticker := time.NewTicker(c.pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				if parent.Err() != nil {
					// The program is stopping, not the connection failing:
					// tell the server so it need not wait for a timeout.
					shutdownCtx, cancel := context.WithTimeout(context.Background(), c.pongWait)
					conn.Shutdown(shutdownCtx, websocket.StatusGoingAway, "client exiting")
					cancel()
				}
				conn.Close()
				return
			case <-ticker.C:
			}
			pingCtx, cancelPing := context.WithTimeout(ctx, c.pongWait)
			err := conn.Ping(pingCtx)
			cancelPing()
			if errors.Is(err, context.DeadlineExceeded) {
				cancel(errPongTimeout)
			}
		}
	}()
	defer func() {
		if cause := context.Cause(ctx); errors.Is(cause, errPongTimeout) {
			err = cause
		}
		cancel(nil)
		<-keepalive
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return received, err
		}
		seq, text, ok := parseMessage(data)
		if !ok {
			c.logf("ignoring malformed message %.40q", data)
			continue
		}
		if seq <= c.lastSeq {
			continue // Replayed; already delivered.
		}
		c.lastSeq = seq
		received = true
		fmt.Fprintf(c.out, "%s\n", text)
	}
}

// parseMessage splits a "<seq> <text>" message.
func parseMessage(data []byte) (seq uint64, text []byte, ok bool) {
	num, text, ok := bytes.Cut(data, []byte(" "))
	if !ok {
		return 0, nil, false
	}
	seq, err := strconv.ParseUint(string(num), 10, 64)
	return seq, text, err == nil
}

// retryableDial reports whether a failed dial may succeed if tried again.
func retryableDial(resp *http.Response, err error) bool {
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.Is(err, websocket.ErrInvalidConfig), errors.As(err, &certErr):
		return false
	case resp == nil:
		// The network, DNS, a proxy or the handshake timed out.
		return true
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooManyRequests:
		return true
	default:
		// 5xx is the server's trouble; other 4xx is the request's, and
		// a server that answers but will not upgrade is misconfigured.
		return resp.StatusCode >= 500
	}
}

// retryableSession reports whether a connection that ended with err should be
// replaced with a new one.
func retryableSession(err error) bool {
	switch websocket.CloseStatus(err) {
	case -1:
		// No Close frame: the network failed, the server vanished or
		// stopped answering Pings. Only a protocol violation is final.
		return !errors.Is(err, websocket.ErrProtocol)
	case websocket.StatusNormalClosure,
		websocket.StatusProtocolError,
		websocket.StatusUnsupportedData,
		websocket.StatusInvalidFramePayloadData,
		websocket.StatusPolicyViolation,
		websocket.StatusMessageTooBig,
		websocket.StatusMandatoryExtension:
		return false
	default:
		// StatusGoingAway, StatusInternalServerError, StatusServiceRestart,
		// StatusTryAgainLater, StatusBadGateway and application codes.
		return true
	}
}

// retryAfter returns the delay a 429 or 503 response asks for, or zero.
// https://www.rfc-editor.org/rfc/rfc9110#section-10.2.3
func retryAfter(resp *http.Response) time.Duration {
	if resp == nil || (resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable) {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if secs, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// backoff computes reconnect delays: a uniformly random duration up to an
// exponentially growing bound, min doubled per attempt and capped at max.
// min must be positive.
// https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/
type backoff struct {
	min, max time.Duration
}

// delay returns the wait before retry number attempt, counting from zero.
func (b backoff) delay(attempt int) time.Duration {
	bound := b.max
	if attempt < 32 {
		bound = min(b.min<<attempt, b.max)
	}
	return rand.N(bound) + 1
}
