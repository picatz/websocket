// Command auth is an echo server that authenticates every connection before
// upgrading it, and ends each one when its credential expires.
//
// Authentication happens in plain HTTP, before Upgrade, so a refusal is an
// ordinary 401 response with a WWW-Authenticate challenge (RFC 6750) that
// clients and proxies understand, and no WebSocket is ever opened for an
// unknown caller. The server accepts two credentials:
//
//   - An OAuth 2.0 bearer token in the Authorization header, for programs that
//     can set request headers, such as this package's Dialer.
//   - A ticket in the "ticket" query parameter, for browsers, whose WebSocket
//     API cannot set headers. A client exchanges its bearer token for a ticket
//     with POST /ticket, then connects to /ws?ticket=... A ticket is random,
//     works once and expires after 30 seconds, which limits the harm of the
//     URL turning up in a proxy or server log.
//
// Cookies would also reach the handshake, but a browser sends them with
// requests from any site, so cookie authentication is safe only together with
// Upgrader's Origin check; this server keeps the default same-origin policy
// either way.
//
// A credential's expiry is honored for the life of the connection: when it
// passes, the server closes with StatusPolicyViolation and the reason
// "credential expired", and the client must reconnect with a fresh one.
//
// The bearer tokens here are a fixed table so the example stays small and
// dependency-free. A real server verifies a signed token instead, such as an
// OpenID Connect ID token or a JWT access token, by checking its signature
// against the issuer's published keys and its issuer, audience and expiry,
// and keeps the rest of this program as it is.
//
//	go run ./examples/auth
//	go run ./examples/client -H "Authorization: Bearer alice-token" ws://localhost:8080/ws
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/picatz/websocket"
)

// identity is an authenticated caller.
type identity struct {
	subject string
	// expires is when the credential stops being valid.
	expires time.Time
}

// errUnauthenticated is returned for a missing, unknown or expired
// credential.
var errUnauthenticated = errors.New("invalid or expired credential")

// verifyFunc checks a bearer token. A real server verifies a signed token
// here; see the package comment.
type verifyFunc func(ctx context.Context, token string) (identity, error)

// staticTokens returns a verifyFunc for a fixed table of tokens.
func staticTokens(tokens map[string]identity, now func() time.Time) verifyFunc {
	return func(ctx context.Context, token string) (identity, error) {
		id, ok := tokens[token]
		if !ok || !now().Before(id.expires) {
			return identity{}, errUnauthenticated
		}
		return id, nil
	}
}

// ticketTTL is how long a ticket stays valid.
const ticketTTL = 30 * time.Second

// tickets issues single-use tickets for browsers.
type tickets struct {
	now func() time.Time

	mu     sync.Mutex
	issued map[string]ticket
}

// ticket is an issued, unused ticket.
type ticket struct {
	id      identity
	expires time.Time
}

func newTickets(now func() time.Time) *tickets {
	return &tickets{now: now, issued: make(map[string]ticket)}
}

// issue returns a new ticket for id. The ticket expires after ticketTTL, or
// when id's credential does, whichever is sooner.
func (t *tickets) issue(id identity) string {
	value := rand.Text()
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	// Forget expired tickets so that unused ones cannot pile up.
	for k, v := range t.issued {
		if !now.Before(v.expires) {
			delete(t.issued, k)
		}
	}
	expires := now.Add(ticketTTL)
	if id.expires.Before(expires) {
		expires = id.expires
	}
	t.issued[value] = ticket{id: id, expires: expires}
	return value
}

// redeem consumes value and returns the identity it was issued to.
func (t *tickets) redeem(value string) (identity, error) {
	t.mu.Lock()
	tk, ok := t.issued[value]
	delete(t.issued, value)
	t.mu.Unlock()
	if !ok || !t.now().Before(tk.expires) {
		return identity{}, errUnauthenticated
	}
	return tk.id, nil
}

// server authenticates requests and serves WebSocket echo connections.
type server struct {
	verify   verifyFunc
	tickets  *tickets
	upgrader websocket.Upgrader
	now      func() time.Time
}

func newServer(verify verifyFunc, now func() time.Time) *server {
	return &server{
		verify:   verify,
		tickets:  newTickets(now),
		upgrader: websocket.Upgrader{MaxMessageSize: 64 << 10},
		now:      now,
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ticket", s.handleTicket)
	mux.HandleFunc("GET /ws", s.handleWebSocket)
	return mux
}

// bearerToken returns the token of an "Authorization: Bearer" header, or "".
// https://www.rfc-editor.org/rfc/rfc6750#section-2.1
func bearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// unauthorized writes a 401 with a Bearer challenge. A request that carried
// a credential is told it was invalid; one without is only challenged.
// https://www.rfc-editor.org/rfc/rfc6750#section-3
func unauthorized(w http.ResponseWriter, presented bool) {
	challenge := `Bearer realm="websocket"`
	if presented {
		challenge += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// authenticate returns the caller of r, from its bearer token or, when
// allowTicket is set, its ticket.
func (s *server) authenticate(r *http.Request, allowTicket bool) (identity, bool, error) {
	if token := bearerToken(r); token != "" {
		id, err := s.verify(r.Context(), token)
		return id, true, err
	}
	if value := r.URL.Query().Get("ticket"); allowTicket && value != "" {
		id, err := s.tickets.redeem(value)
		return id, true, err
	}
	return identity{}, false, errUnauthenticated
}

// handleTicket exchanges a bearer token for a ticket.
func (s *server) handleTicket(w http.ResponseWriter, r *http.Request) {
	id, presented, err := s.authenticate(r, false)
	if err != nil {
		unauthorized(w, presented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"ticket":     s.tickets.issue(id),
		"expires_in": int(ticketTTL / time.Second),
	})
}

// handleWebSocket authenticates the request, upgrades it and echoes messages
// until the connection ends or the credential expires.
func (s *server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	id, presented, err := s.authenticate(r, true)
	if err != nil {
		unauthorized(w, presented)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r)
	if err != nil {
		// Upgrade has already written the HTTP error response, such as
		// 403 for a cross-origin request.
		return
	}
	defer conn.Close()

	// Close the connection when the credential expires, telling the client
	// why. Closing also ends the read loop below.
	expiry := time.AfterFunc(id.expires.Sub(s.now()), func() {
		conn.CloseWithStatus(websocket.StatusPolicyViolation, "credential expired")
	})
	defer expiry.Stop()

	ctx := r.Context()
	if err := conn.WriteMessage(ctx, websocket.TextMessage, fmt.Appendf(nil, "hello, %s", id.subject)); err != nil {
		return
	}
	for {
		messageType, data, err := conn.ReadMessage(ctx)
		if err != nil {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = conn.WriteMessage(writeCtx, messageType, data)
		cancel()
		if err != nil {
			return
		}
	}
}

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	// Demo tokens; see the package comment.
	expires := time.Now().Add(time.Hour)
	verify := staticTokens(map[string]identity{
		"alice-token": {subject: "alice", expires: expires},
		"bob-token":   {subject: "bob", expires: expires},
	}, time.Now)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           newServer(verify, time.Now).routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("auth server listening on ws://%s/ws", *addr)
	log.Fatal(srv.ListenAndServe())
}
