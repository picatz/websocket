package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// script is a test server whose nth connection attempt, counting from zero,
// is handled by steps[n].
type script struct {
	t     *testing.T
	steps []func(w http.ResponseWriter, r *http.Request)

	mu      sync.Mutex
	attempt int
	resumes []string // Each attempt's Resume-After header.
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := s.attempt
	s.attempt++
	s.resumes = append(s.resumes, r.Header.Get("Resume-After"))
	s.mu.Unlock()
	if n >= len(s.steps) {
		s.t.Errorf("unexpected attempt %d", n+1)
		http.Error(w, "no more steps", http.StatusGone)
		return
	}
	s.steps[n](w, r)
}

// attempts returns the Resume-After header of every attempt so far.
func (s *script) attempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.resumes...)
}

// serve upgrades the request, sends messages as "<seq> <text>" and then runs
// end on the connection.
func serve(first int, texts []string, end func(*websocket.Conn)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		for i, text := range texts {
			if err := c.WriteMessage(context.Background(), websocket.TextMessage, fmt.Appendf(nil, "%d %s", first+i, text)); err != nil {
				return
			}
		}
		end(c)
	}
}

// closeWith ends a connection with a Close frame and waits for the client's
// reply.
func closeWith(code websocket.StatusCode) func(*websocket.Conn) {
	return func(c *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.Shutdown(ctx, code, "")
	}
}

// newTestClient returns a client of srv with short timings, writing messages
// to out.
func newTestClient(t *testing.T, srv *httptest.Server, out io.Writer) *client {
	return &client{
		url:          "ws" + strings.TrimPrefix(srv.URL, "http"),
		backoff:      backoff{min: time.Millisecond, max: 10 * time.Millisecond},
		pingInterval: time.Hour,
		pongWait:     time.Second,
		out:          out,
		logf:         t.Logf,
	}
}

// runClient runs c with a deadline, failing the test if it does not return.
func runClient(t *testing.T, c *client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := c.run(ctx)
	if ctx.Err() != nil {
		t.Fatal("run did not return before its deadline")
	}
	return err
}

// TestResumesAcrossFailures walks the client through a refused handshake, a
// server restart and a replay that overlaps what it already has, and checks
// that every message is printed exactly once, in order.
func TestResumesAcrossFailures(t *testing.T) {
	s := &script{t: t, steps: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "starting up", http.StatusServiceUnavailable)
		},
		serve(1, []string{"one", "two"}, closeWith(websocket.StatusServiceRestart)),
		// The replay starts before what the client asked for.
		serve(2, []string{"two", "three"}, closeWith(websocket.StatusGoingAway)),
		serve(4, []string{"four"}, closeWith(websocket.StatusNormalClosure)),
	}}
	srv := httptest.NewServer(s)
	defer srv.Close()

	var out strings.Builder
	if err := runClient(t, newTestClient(t, srv, &out)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "one\ntwo\nthree\nfour\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if got, want := strings.Join(s.attempts(), ","), "0,0,2,3"; got != want {
		t.Errorf("Resume-After headers = %s, want %s", got, want)
	}
}

// TestGivesUp checks that failures retrying cannot fix end run at once.
func TestGivesUp(t *testing.T) {
	cases := map[string]struct {
		step func(http.ResponseWriter, *http.Request)
		want func(error) bool
	}{
		"unauthorized": {
			step: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "who are you", http.StatusUnauthorized)
			},
			want: func(err error) bool { return errors.Is(err, websocket.ErrBadHandshake) },
		},
		"not a websocket endpoint": {
			step: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("hello"))
			},
			want: func(err error) bool { return errors.Is(err, websocket.ErrBadHandshake) },
		},
		"policy violation": {
			step: serve(1, nil, closeWith(websocket.StatusPolicyViolation)),
			want: func(err error) bool { return websocket.CloseStatus(err) == websocket.StatusPolicyViolation },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &script{t: t, steps: []func(http.ResponseWriter, *http.Request){tc.step}}
			srv := httptest.NewServer(s)
			defer srv.Close()
			var out strings.Builder
			if err := runClient(t, newTestClient(t, srv, &out)); !tc.want(err) {
				t.Fatalf("run error = %v", err)
			}
			if n := len(s.attempts()); n != 1 {
				t.Fatalf("%d attempts, want 1", n)
			}
		})
	}
}

// TestReconnectsAfterSilentServer checks that a server which stops answering
// Pings, as a dead NAT mapping would make it look, is replaced with a new
// connection.
func TestReconnectsAfterSilentServer(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	s := &script{t: t, steps: []func(http.ResponseWriter, *http.Request){
		// Never read, so never answer a Ping.
		serve(1, []string{"one"}, func(*websocket.Conn) { <-stuck }),
		serve(2, []string{"two"}, closeWith(websocket.StatusNormalClosure)),
	}}
	srv := httptest.NewServer(s)
	defer srv.Close()

	var out strings.Builder
	c := newTestClient(t, srv, &out)
	c.pingInterval, c.pongWait = 20*time.Millisecond, 50*time.Millisecond
	if err := runClient(t, c); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "one\ntwo\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// TestStopsOnCancel checks that canceling run's context closes the
// connection with StatusGoingAway, which the server sees, and returns nil.
func TestStopsOnCancel(t *testing.T) {
	serverSaw := make(chan error, 1)
	s := &script{t: t, steps: []func(http.ResponseWriter, *http.Request){
		serve(1, []string{"one"}, func(c *websocket.Conn) {
			_, _, err := c.ReadMessage(context.Background())
			serverSaw <- err
		}),
	}}
	srv := httptest.NewServer(s)
	defer srv.Close()

	var out syncBuilder
	c := newTestClient(t, srv, nil)
	c.out = &out
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.run(ctx) }()
	for out.String() != "one\n" {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if err := <-serverSaw; websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("server read error = %v, want StatusGoingAway", err)
	}
}

// syncBuilder is a strings.Builder safe for one writer and one reader.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuilder) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuilder) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestBackoffDelay(t *testing.T) {
	b := backoff{min: 100 * time.Millisecond, max: time.Second}
	for attempt, bound := range []time.Duration{100, 200, 400, 800, 1000, 1000} {
		bound *= time.Millisecond
		for range 100 {
			if d := b.delay(attempt); d <= 0 || d > bound {
				t.Fatalf("delay(%d) = %v, want in (0, %v]", attempt, d, bound)
			}
		}
	}
	if d := b.delay(1000); d <= 0 || d > b.max {
		t.Fatalf("delay(1000) = %v, want in (0, %v]", d, b.max)
	}
}

func TestRetryAfter(t *testing.T) {
	resp := func(status int, value string) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {value}}}
	}
	for _, tc := range []struct {
		resp *http.Response
		want time.Duration
	}{
		{nil, 0},
		{resp(http.StatusServiceUnavailable, "3"), 3 * time.Second},
		{resp(http.StatusTooManyRequests, "1"), time.Second},
		{resp(http.StatusBadGateway, "3"), 0},
		{resp(http.StatusServiceUnavailable, "soon"), 0},
		{resp(http.StatusServiceUnavailable, "Sun, 06 Nov 1994 08:49:37 GMT"), 0},
	} {
		if got := retryAfter(tc.resp); got != tc.want {
			t.Errorf("retryAfter(%v) = %v, want %v", tc.resp, got, tc.want)
		}
	}
}
