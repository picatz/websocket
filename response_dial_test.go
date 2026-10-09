package websocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func responseHeadFor(r *http.Request, reason, fields string) string {
	return "HTTP/1.1 101 " + reason + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n" + fields + "\r\n"
}
func responseHeadOfSize(r *http.Request, size int) string {
	head := responseHeadFor(r, "x", "X-Pad: \r\n")
	return responseHeadFor(r, "x", "X-Pad: "+strings.Repeat("x", size-len(head))+"\r\n")
}

type responsePeerResult struct {
	writeErr, readErr error
	readBytes         int
}

func responsePeer(t *testing.T, tls bool, wire func(*http.Request) []byte, sent ...chan struct{}) (string, []DialOption, <-chan responsePeerResult) {
	t.Helper()
	done := make(chan responsePeerResult, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			done <- responsePeerResult{writeErr: err}
			return
		}
		defer raw.Close()
		if err := raw.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			done <- responsePeerResult{writeErr: err}
			return
		}
		_, writeErr := raw.Write(wire(r))
		for _, ready := range sent {
			close(ready)
		}
		var b [1]byte
		n, readErr := raw.Read(b[:])
		done <- responsePeerResult{writeErr, readErr, n}
	}))
	var opts []DialOption
	if tls {
		srv.StartTLS()
		opts = append(opts, WithTLSConfig(srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()))
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), opts, done
}
func responseWait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for bounded peer")
		var zero T
		return zero
	}
}
func responsePeerClosed(t *testing.T, ch <-chan responsePeerResult, writeOK bool) {
	t.Helper()
	result := responseWait(t, ch)
	if writeOK && result.writeErr != nil {
		t.Fatal(result.writeErr)
	}
	var ne net.Error
	if result.readBytes != 0 || result.readErr == nil || (errors.As(result.readErr, &ne) && ne.Timeout()) {
		t.Fatalf("transport not closed: %+v", result)
	}
}
func responseDial(t *testing.T, url string, opts ...DialOption) (*Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return Dial(ctx, url, opts...)
}

func TestDialResponseHeadLimitOptions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		size          int
		opts          []DialOption
		wantSizeError bool
	}{
		{"default_exact", DefaultMaxResponseHeaderBytes, nil, false},
		{"default_plus_one", DefaultMaxResponseHeaderBytes + 1, nil, true},
		{"nil_retains_default", DefaultMaxResponseHeaderBytes + 1, []DialOption{nil}, true},
		{"positive_exact", 4096, []DialOption{WithMaxResponseHeaderBytes(4096)}, false},
		{"positive_plus_one", 4097, []DialOption{WithMaxResponseHeaderBytes(4096)}, true},
		{"larger_positive", DefaultMaxResponseHeaderBytes + 1, []DialOption{WithMaxResponseHeaderBytes(DefaultMaxResponseHeaderBytes + 1)}, false},
		{"zero_unlimited", DefaultMaxResponseHeaderBytes + 1, []DialOption{WithMaxResponseHeaderBytes(0)}, false},
		{"negative_unlimited", DefaultMaxResponseHeaderBytes + 1, []DialOption{WithMaxResponseHeaderBytes(-1)}, false},
		{"last_unlimited", 4097, []DialOption{WithMaxResponseHeaderBytes(1), nil, WithMaxResponseHeaderBytes(0)}, false},
		{"last_positive", 4097, []DialOption{WithMaxResponseHeaderBytes(0), WithMaxResponseHeaderBytes(4096), nil}, true},
		{"independent_tiny_message", 65536, []DialOption{WithMaxMessageSize(1)}, false},
		{"independent_unlimited_message", 4097, []DialOption{WithMaxMessageSize(0), WithMaxResponseHeaderBytes(4096)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, opts, closed := responsePeer(t, false, func(r *http.Request) []byte { return append([]byte(responseHeadOfSize(r, tc.size)), 0x82, 1, 'x') })
			c, resp, err := responseDial(t, url, append(opts, tc.opts...)...)
			if tc.wantSizeError {
				if c != nil || resp != nil || !errors.Is(err, ErrBadHandshake) || !errors.Is(err, ErrResponseHeaderTooLarge) || errors.Is(err, ErrPayloadTooLarge) {
					t.Fatalf("connection=%v response=%v error=%v", c, resp, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer c.conn.Close()
				if len(resp.Header.Get("X-Pad")) != tc.size-len(responseHeadFor(&http.Request{Header: http.Header{}}, "x", "X-Pad: \r\n")) {
					t.Fatal("unexpected head size")
				}
				if resp.Body != http.NoBody {
					t.Fatalf("101 body=%T", resp.Body)
				}
				resp.Body.Close()
				op, p, err := c.ReadMessage()
				if err != nil || op != BinaryMessage || string(p) != "x" {
					t.Fatalf("first frame=%v %q %v", op, p, err)
				}
				c.conn.Close()
			}
			responsePeerClosed(t, closed, !tc.wantSizeError)
		})
	}
}

func TestDialResponseHeadTCPAndTLSBoundaries(t *testing.T) {
	for _, tls := range []bool{false, true} {
		for _, size := range []int{256, 4095, 4096, 4097} {
			for _, offset := range []int{-1, 0, 1} {
				t.Run(fmt.Sprintf("tls=%v/head=%d/offset=%d", tls, size, offset), func(t *testing.T) {
					payload := bytes.Repeat([]byte("frame"), 16384)
					frame := []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 0, 0}
					binary.BigEndian.PutUint64(frame[2:], uint64(len(payload)))
					frame = append(frame, payload...)
					url, opts, closed := responsePeer(t, tls, func(r *http.Request) []byte { return append([]byte(responseHeadOfSize(r, size)), frame...) })
					c, resp, err := responseDial(t, url, append(opts, WithMaxResponseHeaderBytes(int64(size+offset)))...)
					if offset < 0 {
						if c != nil || resp != nil || !errors.Is(err, ErrBadHandshake) || !errors.Is(err, ErrResponseHeaderTooLarge) {
							t.Fatalf("connection=%v response=%v error=%v", c, resp, err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						defer c.conn.Close()
						resp.Body.Close()
						_, got, err := c.ReadMessage()
						if err != nil || !bytes.Equal(got, payload) {
							t.Fatalf("first frame length=%d error=%v", len(got), err)
						}
						c.conn.Close()
					}
					responsePeerClosed(t, closed, offset >= 0)
				})
			}
		}
	}
}

func TestDialResponseHeadCountsStatusAndFields(t *testing.T) {
	for _, tc := range []struct {
		name, reason, fields string
		check                func(*testing.T, *http.Response)
	}{
		{"status", strings.Repeat("s", 8192), "", func(t *testing.T, r *http.Response) {
			if len(r.Status) != 8196 {
				t.Fatal("status changed")
			}
		}},
		{"long_value", "x", "X: " + strings.Repeat("v", 8192) + "\r\n", func(t *testing.T, r *http.Response) {
			if len(r.Header.Get("X")) != 8192 {
				t.Fatal("field changed")
			}
		}},
		{"duplicates", "x", strings.Repeat("X: x\r\n", 8192), func(t *testing.T, r *http.Response) {
			if len(r.Header.Values("X")) != 8192 {
				t.Fatal("duplicates changed")
			}
		}},
		{"folds", "x", "X: one\r\n two\r\n\tthree\r\n", func(t *testing.T, r *http.Response) {
			if r.Header.Get("X") != "one two three" {
				t.Fatal("folded value changed")
			}
		}},
	} {
		for _, offset := range []int{-1, 0} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, offset), func(t *testing.T) {
				sample := responseHeadFor(&http.Request{Header: http.Header{}}, tc.reason, tc.fields)
				url, opts, closed := responsePeer(t, false, func(r *http.Request) []byte { return []byte(responseHeadFor(r, tc.reason, tc.fields)) })
				c, resp, err := responseDial(t, url, append(opts, WithMaxResponseHeaderBytes(int64(len(sample)+offset)), WithMaxMessageSize(1))...)
				if offset < 0 {
					if c != nil || resp != nil || !errors.Is(err, ErrResponseHeaderTooLarge) {
						t.Fatalf("response=%v error=%v", resp, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					tc.check(t, resp)
					c.conn.Close()
				}
				responsePeerClosed(t, closed, offset >= 0)
			})
		}
	}
}

func TestDialResponseHeadValidationAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(string) string
		want   error
		status int
	}{
		{"connection", func(s string) string { return strings.Replace(s, "Connection: Upgrade", "Connection: close", 1) }, ErrInvalidConnectionHeader, 101},
		{"upgrade", func(s string) string { return strings.Replace(s, "Upgrade: websocket", "Upgrade: http", 1) }, ErrInvalidUpgradeHeader, 101},
		{"accept", func(s string) string {
			return strings.Replace(s, "Sec-WebSocket-Accept:", "Sec-WebSocket-Accept: invalid", 1)
		}, ErrInvalidSecAccept, 101},
		{"duplicate_accept", func(s string) string {
			return strings.Replace(s, "\r\n\r\n", "\r\nSec-WebSocket-Accept: duplicate\r\n\r\n", 1)
		}, ErrInvalidSecAccept, 101},
		{"subprotocol", func(s string) string {
			return strings.Replace(s, "\r\n\r\n", "\r\nSec-WebSocket-Protocol: unoffered\r\n\r\n", 1)
		}, ErrInvalidSubprotocol, 101},
		{"extension", func(s string) string {
			return strings.Replace(s, "\r\n\r\n", "\r\nSec-WebSocket-Extensions: unoffered\r\n\r\n", 1)
		}, ErrInvalidExtension, 101},
		{"non101", func(s string) string {
			return "HTTP/1.1 403 Forbidden\r\nX: one\r\nx: two\r\nContent-Length: 6\r\n\r\n"
		}, ErrBadHandshake, 403},
		{"informational", func(s string) string { return "HTTP/1.1 100 Continue\r\n\r\n" }, ErrBadHandshake, 100},
	} {
		for _, offset := range []int{-1, 0, 8} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, offset), func(t *testing.T) {
				sample := tc.change(responseHeadFor(&http.Request{Header: http.Header{}}, "x", ""))
				url, opts, closed := responsePeer(t, false, func(r *http.Request) []byte { return []byte(tc.change(responseHeadFor(r, "x", "")) + "denied") })
				c, resp, err := responseDial(t, url, append(opts, WithMaxResponseHeaderBytes(int64(len(sample)+offset)))...)
				if c != nil {
					c.conn.Close()
					t.Fatal("rejected handshake returned connection")
				}
				if offset < 0 {
					if resp != nil || !errors.Is(err, ErrBadHandshake) || !errors.Is(err, ErrResponseHeaderTooLarge) {
						t.Fatalf("response=%v error=%v", resp, err)
					}
				} else {
					if resp == nil || resp.StatusCode != tc.status || !errors.Is(err, tc.want) || errors.Is(err, ErrResponseHeaderTooLarge) {
						t.Fatalf("response=%v error=%v", resp, err)
					}
					if tc.status == 101 && resp.Body != http.NoBody {
						t.Fatalf("rejected 101 body=%T", resp.Body)
					}
					if tc.status == 403 {
						if strings.Join(resp.Header.Values("X"), ",") != "one,two" {
							t.Fatal("custom duplicate headers changed")
						}
						// Dial closes non-101 transports; only prefetched body bytes may survive.
						// Do not assert complete streaming error bodies after this existing closure.
						b, bodyErr := io.ReadAll(resp.Body)
						if len(b) > 6 || errors.Is(bodyErr, ErrResponseHeaderTooLarge) {
							t.Fatalf("body=%q error=%v", b, bodyErr)
						}
					}
					resp.Body.Close()
				}
				responsePeerClosed(t, closed, offset >= 0)
			})
		}
	}
}

func TestDialResponseHeadCancellation(t *testing.T) {
	for _, tls := range []bool{false, true} {
		for _, prefix := range []string{"HTTP/1.1 101 " + strings.Repeat("s", 8192), "HTTP/1.1 101 x\r\n" + strings.Repeat("X: x\r\n", 1024)} {
			t.Run(fmt.Sprintf("tls=%v/length=%d", tls, len(prefix)), func(t *testing.T) {
				sent := make(chan struct{})
				url, opts, closed := responsePeer(t, tls, func(r *http.Request) []byte { return []byte(prefix) }, sent)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				type result struct {
					c   *Conn
					r   *http.Response
					err error
				}
				done := make(chan result, 1)
				go func() {
					c, r, err := Dial(ctx, url, append(opts, WithMaxResponseHeaderBytes(int64(len(prefix)+1)))...)
					done <- result{c, r, err}
				}()
				responseWait(t, sent)
				cancel()
				got := responseWait(t, done)
				if got.c != nil || got.r != nil || !errors.Is(got.err, ErrBadHandshake) || !errors.Is(got.err, context.Canceled) || errors.Is(got.err, ErrResponseHeaderTooLarge) {
					t.Fatalf("result=%+v", got)
				}
				responsePeerClosed(t, closed, false)
			})
		}
	}
}
