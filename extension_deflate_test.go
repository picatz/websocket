package websocket

import (
	"bufio"
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestPerMessageDeflateExactSelection(t *testing.T) {
	for _, tc := range []struct {
		header  string
		enabled bool
		invalid bool
	}{
		{"", false, false},
		{"permessage-deflate", true, false},
		{"x-permessage-deflate", false, false},
		{"permessage-deflate-x", false, false},
		{"PERMESSAGE-DEFLATE", false, false},
		{"other; permessage-deflate", false, false},
		{"other; value=permessage-deflate", false, false},
		{`other; value="permessage-deflate"`, false, false},
		{"other; server_max_window_bits=8, permessage-deflate", true, false},
		{`permessage-deflate; server_max_window_bits="1\5"`, true, false},
		{"permessage-deflate; client_no_context_takeover; server_no_context_takeover", true, false},
		{"permessage-deflate, permessage-deflate", false, true},
		{"permessage-deflate; server_max_window_bits=10, permessage-deflate; server_max_window_bits=15", false, true},
		{"permessage-deflate; server_max_window_bits=10; server_max_window_bits=15", false, true},
		{"permessage-deflate; client_no_context_takeover; client_no_context_takeover", false, true},
		{"permessage-deflate; unknown", false, true},
		{"permessage-deflate; client_no_context_takeover=1", false, true},
		{"permessage-deflate; server_no_context_takeover=1", false, true},
		{"permessage-deflate; client_max_window_bits", false, true},
		{"permessage-deflate; server_max_window_bits", false, true},
		{"permessage-deflate; client_max_window_bits=7", false, true},
		{"permessage-deflate; server_max_window_bits=16", false, true},
		{"permessage-deflate; server_max_window_bits=09", false, true},
		{"permessage-deflate; server_max_window_bits=+9", false, true},
		{"permessage-deflate; server_max_window_bits=99999999999999999999999", false, true},
		{"permessage-deflate;", false, true},
	} {
		t.Run(tc.header, func(t *testing.T) {
			pmd := NewPerMessageDeflateExtension().(*perMessageDeflate)
			err := pmd.Negotiate(tc.header)
			if (err != nil) != tc.invalid || pmd.enabled != tc.enabled || err != nil && !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("Negotiate(%q) = %v, enabled = %v", tc.header, err, pmd.enabled)
			}
			if tc.header == "other; server_max_window_bits=8, permessage-deflate" && pmd.serverMaxWindowBits != 0 {
				t.Fatal("imported another extension's parameters")
			}
			if err := pmd.Negotiate(""); err != nil || pmd.enabled {
				t.Fatalf("absent selection left extension enabled: %v", err)
			}
		})
	}
}

func TestUpgradePerMessageDeflateAlternatives(t *testing.T) {
	for _, tc := range []struct {
		name      string
		offers    []string
		selection string
	}{
		{"absent", nil, ""},
		{"prefix", []string{"x-permessage-deflate"}, ""},
		{"suffix", []string{"permessage-deflate-x"}, ""},
		{"case", []string{"PerMessage-Deflate"}, ""},
		{"parameter name", []string{"other; permessage-deflate"}, ""},
		{"parameter value", []string{"other; value=permessage-deflate"}, ""},
		{"exact", []string{"permessage-deflate"}, "permessage-deflate"},
		{"second field", []string{"other", "permessage-deflate"}, "permessage-deflate"},
		{"other parameters", []string{"other; server_no_context_takeover; server_max_window_bits=8", "permessage-deflate"}, "permessage-deflate"},
		{"first alternative", []string{"permessage-deflate; server_no_context_takeover, permessage-deflate; client_no_context_takeover"}, "permessage-deflate; server_no_context_takeover"},
		{"skip unknown parameter", []string{"permessage-deflate; server_no_context_takeover; unknown, permessage-deflate"}, "permessage-deflate"},
		{"skip duplicate parameter", []string{"permessage-deflate; server_max_window_bits=10; server_max_window_bits=15", "permessage-deflate"}, "permessage-deflate"},
		{"skip invalid value", []string{"permessage-deflate; server_max_window_bits=16, permessage-deflate; client_no_context_takeover"}, "permessage-deflate; client_no_context_takeover"},
		{"decline all alternatives", []string{"permessage-deflate; unknown, permessage-deflate; client_no_context_takeover=1"}, ""},
		{"optional offer window", []string{"permessage-deflate; client_max_window_bits"}, "permessage-deflate; client_max_window_bits=15"},
		{"quoted window", []string{`permessage-deflate; server_max_window_bits="15"`}, "permessage-deflate; server_max_window_bits=15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := upgradeRequest()
			r.Header["Sec-WebSocket-Extensions"] = tc.offers
			raw := &memoryConn{Reader: bytes.NewReader(nil)}
			w := &hijackResponse{httptest.NewRecorder(), raw, bufio.NewReadWriter(bufio.NewReader(raw), bufio.NewWriter(raw))}
			pmd := NewPerMessageDeflateExtension()
			conn, err := Upgrade(w, r, WithUpgradeExtensions(pmd))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.conn.Close()
			resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw.written.String())), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Header.Get("Sec-WebSocket-Extensions"); got != tc.selection || pmd.IsEnabled() != (tc.selection != "") {
				t.Fatalf("selection = %q, enabled = %v; want %q", got, pmd.IsEnabled(), tc.selection)
			}
		})
	}
}

func TestExtensionQuotedTokenByteAlphabet(t *testing.T) {
	for i := 0; i < 256; i++ {
		allowed := i >= 'a' && i <= 'z' || i >= 'A' && i <= 'Z' || i >= '0' && i <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(i))
		for _, prefix := range []string{"", "\\"} {
			input := `"x` + prefix + string(byte(i)) + `y"`
			value, rest, ok := consumeExtensionQuotedToken(input)
			if i == '\\' && prefix == "" {
				if !ok || rest != "" || value != "xy" {
					t.Fatalf("escaped y = %q, %q, %v", value, rest, ok)
				}
				continue
			}
			if (ok && rest == "") != allowed || allowed && value != "x"+string(byte(i))+"y" {
				t.Fatalf("quoted byte %d, prefix %q: %q, %q, %v", i, prefix, value, rest, ok)
			}
		}
	}
}

func FuzzExtensionNegotiation(f *testing.F) {
	for _, input := range []string{"", "example", "permessage-deflate", "permessage-deflate; client_max_window_bits", "x-permessage-deflate", "example; a=1; a=2", `example; a="1\5"`, `example; a="x,y"`, "permessage-deflate, permessage-deflate", ",,example,,", "example\r\nother"} {
		f.Add(input, "")
	}
	f.Fuzz(func(t *testing.T, first, second string) {
		parsed, err := parseExtensions([]string{first, second})
		if err != nil {
			if !errors.Is(err, ErrInvalidExtension) {
				t.Fatal(err)
			}
			return
		}
		// A successful parse must round-trip with unescaped token values,
		// preserving extension order, alternatives and repeated parameters.
		var normalized []string
		for _, ext := range parsed {
			if !isHTTPToken(ext.name) {
				t.Fatalf("invalid extension token: %q", ext.name)
			}
			s := ext.name
			for _, param := range ext.params {
				if !isHTTPToken(param.name) || param.hasValue && !isHTTPToken(param.value) {
					t.Fatalf("invalid parameter: %#v", param)
				}
				s += "; " + param.name
				if param.hasValue {
					s += "=" + param.value
				}
			}
			normalized = append(normalized, s)
		}
		got, err := parseExtensions([]string{strings.Join(normalized, ", ")})
		if err != nil || !reflect.DeepEqual(got, parsed) {
			t.Fatalf("round trip = %#v, %v; want %#v", got, err, parsed)
		}
		pmd := NewPerMessageDeflateExtension()
		if err := pmd.Negotiate(strings.Join(normalized, ", ")); err != nil && pmd.IsEnabled() {
			t.Fatal("failed negotiation enabled compression")
		}
	})
}
