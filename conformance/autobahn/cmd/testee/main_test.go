package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/picatz/websocket"
)

func TestValidateAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:9001", "127.0.0.2:9001", "[::1]:9001"} {
		if err := validateAddress(address); err != nil {
			t.Errorf("%q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"localhost:9001", "example.com:9001", "0.0.0.0:9001", "[::]:9001", "10.0.0.1:9001", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:http", "127.0.0.1"} {
		if err := validateAddress(address); err == nil {
			t.Errorf("%q accepted", address)
		}
	}
}

func TestCommands(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agent") != "test agent" {
			t.Errorf("agent query = %q", r.URL.RawQuery)
		}
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		switch r.URL.Path {
		case "/getCaseCount":
			_ = conn.WriteMessage(websocket.TextMessage, []byte("247"))
		case "/getCaseStatus":
			if r.URL.Query().Get("case") != "247" {
				t.Errorf("case query = %q", r.URL.RawQuery)
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"behavior":"OK"}`))
		case "/updateReports":
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	count, err := command(address, "/getCaseCount", "test agent", 0)
	if err != nil || string(count) != "247" {
		t.Fatalf("count = %q, %v", count, err)
	}
	if _, err := command(address, "/getCaseStatus", "test agent", 247); err != nil {
		t.Fatal(err)
	}
	if _, err := command(address, "/updateReports", "test agent", 0); err != nil {
		t.Fatal(err)
	}
}
