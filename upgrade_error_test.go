package websocket

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpgradeErrorStatus(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{ErrHandshakeFailed, 0},
		{fmt.Errorf("%w: hijack: broken", ErrHandshakeFailed), 0},
		{ErrOriginNotAllowed, http.StatusForbidden},
		{ErrUnsupportedVersion, http.StatusUpgradeRequired},
		{ErrNotHijacker, http.StatusInternalServerError},
		{fmt.Errorf("%w: %w", ErrInvalidHandshakeHeader, ErrInvalidSubprotocol), http.StatusInternalServerError},
		{ErrInvalidSubprotocol, http.StatusBadRequest}, // from the client's offer
		{ErrInvalidMethod, http.StatusBadRequest},
		{ErrMissingSecKey, http.StatusBadRequest},
		{ErrInvalidExtension, http.StatusBadRequest},
		{errors.New("other"), http.StatusBadRequest},
	} {
		if got := UpgradeErrorStatus(tt.err); got != tt.want {
			t.Errorf("UpgradeErrorStatus(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

// Over a real server, the handler only returns and the client still sees the
// right status, headers and a fixed body that carries no error details.
func TestUpgradeWritesErrorResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, err := Upgrade(w, r); err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()

	for _, tt := range []struct {
		name    string
		header  http.Header
		status  int
		version string
	}{
		{"plain GET", nil, http.StatusBadRequest, ""},
		{"cross origin", http.Header{"Origin": {"https://evil.test"}}, http.StatusForbidden, ""},
		{"old version", http.Header{"Sec-WebSocket-Version": {"8"}}, http.StatusUpgradeRequired, "13"},
		{"bad key", http.Header{"Sec-WebSocket-Key": {"short"}}, http.StatusBadRequest, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			if tt.header != nil {
				req.Header = upgradeRequest().Header
				for k, v := range tt.header {
					req.Header[k] = v
				}
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.status || resp.Header.Get("Sec-WebSocket-Version") != tt.version {
				t.Fatalf("response = %d, version %q; want %d, %q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Version"), tt.status, tt.version)
			}
			if want := "WebSocket: " + http.StatusText(tt.status) + "\n"; string(body) != want {
				t.Fatalf("body = %q, want %q", body, want)
			}
		})
	}
}
