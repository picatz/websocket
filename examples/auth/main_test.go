package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testServer starts a server that knows alice-token, valid for lifetime.
func testServer(t *testing.T, lifetime time.Duration) (*httptest.Server, *clock) {
	t.Helper()
	clk := &clock{t: time.Now()}
	verify := staticTokens(map[string]identity{
		"alice-token": {subject: "alice", expires: clk.now().Add(lifetime)},
	}, clk.now)
	srv := httptest.NewServer(newServer(verify, clk.now).routes())
	t.Cleanup(srv.Close)
	return srv, clk
}

func wsURL(srv *httptest.Server, query string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws" + query
}

// dialAs dials with the given Authorization header value, if any.
func dialAs(t *testing.T, url, authorization string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	var d websocket.Dialer
	if authorization != "" {
		d.Header = http.Header{"Authorization": {authorization}}
	}
	return d.Dial(t.Context(), url)
}

// greet reads the server's greeting and checks one echo.
func greet(t *testing.T, c *websocket.Conn) {
	t.Helper()
	if _, msg, err := c.ReadMessage(t.Context()); err != nil || string(msg) != "hello, alice" {
		t.Fatalf("greeting = %q, %v", msg, err)
	}
	if err := c.WriteMessage(t.Context(), websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := c.ReadMessage(t.Context()); err != nil || string(msg) != "ping" {
		t.Fatalf("echo = %q, %v", msg, err)
	}
}

func TestBearerToken(t *testing.T) {
	srv, _ := testServer(t, time.Hour)
	c, _, err := dialAs(t, wsURL(srv, ""), "Bearer alice-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	greet(t, c)
}

func TestRejectedBeforeUpgrade(t *testing.T) {
	srv, _ := testServer(t, time.Hour)
	for _, tc := range []struct {
		name, authorization, query, challenge string
	}{
		{"no credential", "", "", `Bearer realm="websocket"`},
		{"unknown token", "Bearer mallory-token", "", `Bearer realm="websocket", error="invalid_token"`},
		{"other scheme", "Basic YWxpY2U6c2VjcmV0", "", `Bearer realm="websocket"`},
		{"unknown ticket", "", "?ticket=forged", `Bearer realm="websocket", error="invalid_token"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, resp, err := dialAs(t, wsURL(srv, tc.query), tc.authorization)
			if !errors.Is(err, websocket.ErrBadHandshake) {
				t.Fatalf("err = %v, want ErrBadHandshake", err)
			}
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != tc.challenge {
				t.Fatalf("response %d with challenge %q, want 401 with %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"), tc.challenge)
			}
		})
	}
}

// getTicket exchanges alice's token for a ticket.
func getTicket(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/ticket", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ticket: %d, %v", resp.StatusCode, err)
	}
	if body.Ticket == "" || body.ExpiresIn != 30 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("POST /ticket = %+v, Cache-Control %q", body, resp.Header.Get("Cache-Control"))
	}
	return body.Ticket
}

func TestTicketWorksOnce(t *testing.T) {
	srv, _ := testServer(t, time.Hour)
	ticket := getTicket(t, srv)
	c, _, err := dialAs(t, wsURL(srv, "?ticket="+ticket), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	greet(t, c)

	if _, resp, err := dialAs(t, wsURL(srv, "?ticket="+ticket), ""); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused ticket: err = %v", err)
	}
}

func TestTicketExpires(t *testing.T) {
	srv, clk := testServer(t, time.Hour)
	ticket := getTicket(t, srv)
	clk.advance(ticketTTL)
	if _, resp, err := dialAs(t, wsURL(srv, "?ticket="+ticket), ""); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired ticket: err = %v", err)
	}
}

func TestTicketNeedsBearerToken(t *testing.T) {
	srv, _ := testServer(t, time.Hour)
	resp, err := srv.Client().Post(srv.URL+"/ticket", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /ticket without a token: %d, want 401", resp.StatusCode)
	}
}

// A browser page on another site cannot use a ticket it obtained: the
// default Origin policy refuses the handshake.
func TestCrossOriginRefused(t *testing.T) {
	srv, _ := testServer(t, time.Hour)
	d := websocket.Dialer{Header: http.Header{"Origin": {"https://evil.example"}}}
	_, resp, err := d.Dial(t.Context(), wsURL(srv, "?ticket="+getTicket(t, srv)))
	if !errors.Is(err, websocket.ErrBadHandshake) || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin dial: err = %v", err)
	}
}

func TestClosedWhenCredentialExpires(t *testing.T) {
	srv, _ := testServer(t, 100*time.Millisecond)
	c, _, err := dialAs(t, wsURL(srv, ""), "Bearer alice-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	greet(t, c)

	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = c.ReadMessage(t.Context())
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != "credential expired" {
		t.Fatalf("read error = %v, want StatusPolicyViolation \"credential expired\"", err)
	}
}
