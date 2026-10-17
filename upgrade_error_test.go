package websocket

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
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
		{ErrInvalidMethod, http.StatusMethodNotAllowed},
		{ErrMissingSecKey, http.StatusBadRequest},
		{ErrInvalidExtension, http.StatusBadRequest},
		{errors.New("other"), http.StatusBadRequest},
	} {
		if got := upgradeErrorStatus(tt.err); got != tt.want {
			t.Errorf("upgradeErrorStatus(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

// Over a real server, the handler only returns and the client still sees the
// right status, headers and a fixed body that carries no error details.
func TestUpgradeWritesErrorResponses(t *testing.T) {
	errs := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err == nil {
			conn.Close()
		}
		errs <- err
	}))
	defer srv.Close()

	for _, tt := range []struct {
		name   string
		method string
		header http.Header
		status int
		want   http.Header // response fields that must be present
	}{
		{"plain GET", http.MethodGet, nil, http.StatusBadRequest, nil},
		{"cross origin", http.MethodGet, http.Header{"Origin": {"https://evil.test"}}, http.StatusForbidden, nil},
		{"old version", http.MethodGet, http.Header{"Sec-WebSocket-Version": {"8"}}, http.StatusUpgradeRequired, http.Header{"Sec-Websocket-Version": {"13"}}},
		{"bad key", http.MethodGet, http.Header{"Sec-WebSocket-Key": {"short"}}, http.StatusBadRequest, nil},
		{"POST", http.MethodPost, http.Header{}, http.StatusMethodNotAllowed, http.Header{"Allow": {"GET"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), tt.method, srv.URL, nil)
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
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			for k, v := range tt.want {
				if got := resp.Header.Values(k); !slices.Equal(got, v) {
					t.Fatalf("%s = %q, want %q", k, got, v)
				}
			}
			if tt.want.Get("Sec-WebSocket-Version") == "" && resp.Header.Get("Sec-WebSocket-Version") != "" {
				t.Fatalf("unexpected Sec-WebSocket-Version on %d", resp.StatusCode)
			}
			if want := "WebSocket: " + http.StatusText(tt.status) + "\n"; string(body) != want {
				t.Fatalf("body = %q, want %q", body, want)
			}
			// The client's fault matches ErrBadHandshake; a policy rejection does not.
			err = <-errs
			if bad := tt.status != http.StatusForbidden; errors.Is(err, ErrBadHandshake) != bad || errors.Is(err, ErrOriginNotAllowed) == bad {
				t.Fatalf("Upgrade error = %v; want ErrBadHandshake %v", err, bad)
			}
		})
	}
}

type failingHijacker struct {
	*httptest.ResponseRecorder
	err error
}

func (w failingHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, w.err }

// A hijack failure keeps its cause reachable and writes no response.
func TestUpgradeHijackErrorIsWrapped(t *testing.T) {
	cause := errors.New("hijack broke")
	w := failingHijacker{httptest.NewRecorder(), cause}
	conn, err := Upgrade(w, upgradeRequest())
	if conn != nil || !errors.Is(err, ErrHandshakeFailed) || !errors.Is(err, cause) {
		t.Fatalf("Upgrade = %v, %v; want ErrHandshakeFailed wrapping the cause", conn, err)
	}
	if w.Body.Len() != 0 || w.Code != http.StatusOK {
		t.Fatalf("wrote %d %q after a hijack failure", w.Code, w.Body.String())
	}
}

// Upgrader.Error replaces the body, keeps the required header fields, and
// receives the same error Upgrade returns.
func TestUpgraderErrorHook(t *testing.T) {
	var gotStatus int
	var gotErr error
	u := &Upgrader{Error: func(w http.ResponseWriter, r *http.Request, status int, err error) {
		gotStatus, gotErr = status, err
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"upgrade required"}`))
	}}
	r := upgradeRequest()
	r.Header.Set("Sec-WebSocket-Version", "8")
	w := httptest.NewRecorder()
	conn, err := u.Upgrade(w, r)
	if conn != nil || !errors.Is(err, ErrBadHandshake) || !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Upgrade = %v, %v", conn, err)
	}
	if gotStatus != http.StatusUpgradeRequired || gotErr != err {
		t.Fatalf("hook got %d, %v; want 426 and the returned error", gotStatus, gotErr)
	}
	if w.Code != http.StatusUpgradeRequired || w.Body.String() != `{"error":"upgrade required"}` {
		t.Fatalf("response = %d %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Sec-WebSocket-Version"); got != "13" {
		t.Fatalf("Sec-WebSocket-Version = %q, want 13", got)
	}

	// The hook is not called when nothing is written.
	gotStatus = 0
	if _, err := u.Upgrade(failingHijacker{httptest.NewRecorder(), errors.New("broke")}, upgradeRequest()); !errors.Is(err, ErrHandshakeFailed) || gotStatus != 0 {
		t.Fatalf("hook called with %d for %v", gotStatus, err)
	}
}
