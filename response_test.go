package websocket

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

type responseCountReader struct {
	r       io.Reader
	n       int64
	oneByte bool
}

func (r *responseCountReader) Read(p []byte) (int, error) {
	if r.oneByte && len(p) > 1 {
		p = p[:1]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func TestResponseHeadBoundaries(t *testing.T) {
	unique := new(strings.Builder)
	for i := range 2048 {
		fmt.Fprintf(unique, "X-%d: x\r\n", i)
	}
	heads := []struct{ name, head string }{
		{"crlf", "HTTP/1.1 101 x\r\nX: abc\r\n\r\n"},
		{"bare_lf", "HTTP/1.1 101 x\nX: abc\n\n"},
		{"status", "HTTP/1.1 101 " + strings.Repeat("s", 8192) + "\r\n\r\n"},
		{"value", "HTTP/1.1 101 x\r\nX: " + strings.Repeat("x", 8192) + "\r\n\r\n"},
		{"duplicates", "HTTP/1.1 101 x\r\n" + strings.Repeat("X: x\r\n", 8192) + "\r\n"},
		{"unique_fields", "HTTP/1.1 101 x\r\n" + unique.String() + "\r\n"},
		{"folds", "HTTP/1.1 101 x\r\nX: x\r\n" + strings.Repeat(" \tx\r\n", 256) + "\r\n"},
		{"fold_whitespace", "HTTP/1.1 101 x\r\nX: x\r\n" + strings.Repeat(" ", 8192) + "\r\n\r\n"},
	}
	for _, n := range []int{4095, 4096, 4097} {
		prefix := "HTTP/1.1 101 x\r\nX: "
		heads = append(heads, struct{ name, head string }{fmt.Sprint(n), prefix + strings.Repeat("x", n-len(prefix)-4) + "\r\n\r\n"})
	}
	for _, tc := range heads {
		for _, oneByte := range []bool{false, true} {
			for _, offset := range []int{-1, 0, 1} {
				t.Run(fmt.Sprintf("%s/one_byte=%v/offset=%d", tc.name, oneByte, offset), func(t *testing.T) {
					tail := bytes.Repeat([]byte("frame"), 16384)
					src := &responseCountReader{r: strings.NewReader(tc.head + string(tail)), oneByte: oneByte}
					limit := int64(len(tc.head) + offset)
					resp, br, err := readHandshakeResponse(src, limit)
					if src.n > limit {
						t.Fatalf("read %d bytes with budget %d", src.n, limit)
					}
					if offset < 0 {
						if resp != nil || !errors.Is(err, ErrResponseHeaderTooLarge) {
							t.Fatalf("response=%v error=%v", resp, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if resp.Body != http.NoBody {
						t.Fatalf("101 body=%T", resp.Body)
					}
					if err := resp.Body.Close(); err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(br)
					if err != nil || !bytes.Equal(got, tail) {
						t.Fatalf("tail length=%d error=%v", len(got), err)
					}
				})
			}
		}
	}
}

func TestResponseHeadEveryCut(t *testing.T) {
	head := "HTTP/1.1 101 x\r\nX: one\r\nx: two\r\n\r\n"
	for _, oneByte := range []bool{false, true} {
		for n := 1; n < len(head); n++ {
			src := &responseCountReader{r: strings.NewReader(head), oneByte: oneByte}
			resp, _, err := readHandshakeResponse(src, int64(n))
			if resp != nil || !errors.Is(err, ErrResponseHeaderTooLarge) || src.n != int64(n) {
				t.Fatalf("one_byte=%v cut=%d read=%d response=%v error=%v", oneByte, n, src.n, resp, err)
			}
		}
	}
	for _, bad := range []string{"HTTP/1.1 bad\r\n\r\n", "HTTP/1.1 101 x\r\nBad\r\n\r\n"} {
		_, _, err := readHandshakeResponse(strings.NewReader(bad), int64(len(bad)))
		if err == nil || errors.Is(err, ErrResponseHeaderTooLarge) {
			t.Fatalf("complete malformed input mislabeled: %v", err)
		}
	}
}

type responseFailBeyondPrefix struct {
	t         *testing.T
	remaining []byte
}

func (r *responseFailBeyondPrefix) Read(p []byte) (int, error) {
	if len(r.remaining) == 0 {
		r.t.Error("read beyond the bounded prefix")
		return 0, io.EOF
	}
	n := copy(p, r.remaining)
	r.remaining = r.remaining[n:]
	return n, nil
}
func TestResponseHeadNeedsNoExtraPeerByte(t *testing.T) {
	for _, prefix := range []string{
		"HTTP/1.1 101 " + strings.Repeat("s", 8192),
		"HTTP/1.1 101 x\r\nX: " + strings.Repeat("x", 8192),
		"HTTP/1.1 101 x\r\n" + strings.Repeat("X: \r\n", 1024),
		"HTTP/1.1 101 x\r\nX: x\r\n" + strings.Repeat(" ", 8192),
	} {
		src := &responseFailBeyondPrefix{t, []byte(prefix)}
		resp, _, err := readHandshakeResponse(src, int64(len(prefix)))
		if resp != nil || !errors.Is(err, ErrResponseHeaderTooLarge) || len(src.remaining) != 0 {
			t.Fatalf("response=%v error=%v remaining=%d", resp, err, len(src.remaining))
		}
	}
}

func TestResponseHeadBodySemantics(t *testing.T) {
	for _, tc := range []struct{ name, head, body string }{
		{"length", "HTTP/1.1 403 Forbidden\r\nX: one\r\nx: two\r\nContent-Length: 6\r\n\r\n", "denied"},
		{"chunked", "HTTP/1.1 403 Forbidden\r\nTransfer-Encoding: chunked\r\nTrailer: X-End\r\n\r\n", "6\r\ndenied\r\n0\r\nX-End: yes\r\n\r\n"},
		{"until_eof", "HTTP/1.0 403 Forbidden\r\n\r\n", "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _, err := readHandshakeResponse(strings.NewReader(tc.head+tc.body), int64(len(tc.head)))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil || string(b) != "denied" {
				t.Fatalf("body=%q error=%v", b, err)
			}
			if tc.name == "length" && strings.Join(resp.Header.Values("X"), ",") != "one,two" {
				t.Fatal("duplicate header values changed")
			}
			if tc.name == "chunked" && resp.Trailer.Get("X-End") != "yes" {
				t.Fatal("trailer changed")
			}
		})
	}
	for _, fields := range []string{"", "Content-Length: 999\r\n", "Transfer-Encoding: chunked\r\n"} {
		head := "HTTP/1.1 101 x\r\n" + fields + "\r\n"
		resp, br, err := readHandshakeResponse(strings.NewReader(head+"frame"), int64(len(head)))
		if err != nil || resp.Body != http.NoBody {
			t.Fatalf("101 response=%v error=%v", resp, err)
		}
		resp.Body.Close()
		b, err := io.ReadAll(br)
		if err != nil || string(b) != "frame" {
			t.Fatalf("101 tail=%q error=%v", b, err)
		}
	}
}

type responseFinalErrorReader struct {
	data []byte
	err  error
}

func (r *responseFinalErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}
func TestResponseHeadPartialReadErrors(t *testing.T) {
	head := "HTTP/1.1 101 x\r\nX: one\r\n\r\n"
	cause := errors.New("fixture transport failure")
	for _, cause := range []error{cause, io.EOF, io.ErrUnexpectedEOF} {
		for cut := 0; cut <= len(head); cut++ {
			for _, extra := range []int64{0, 1, 4096} {
				limit := int64(cut) + extra
				// Zero is unlimited, so this also exercises unlimited early errors.
				src := &responseFinalErrorReader{[]byte(head[:cut]), cause}
				resp, br, err := readHandshakeResponse(src, limit)
				if cut < len(head) {
					if resp != nil || !errors.Is(err, cause) || errors.Is(err, ErrResponseHeaderTooLarge) {
						t.Fatalf("cut=%d extra=%d cause=%v response=%v error=%v", cut, extra, cause, resp, err)
					}
				} else {
					if err != nil || resp == nil {
						t.Fatalf("complete head with final error: %v", err)
					}
					_, err = br.ReadByte()
					if !errors.Is(err, cause) {
						t.Fatalf("post-head cause=%v want %v", err, cause)
					}
				}
			}
		}
	}
}

func TestResponseHeadUnlimitedAndReaderEdges(t *testing.T) {
	head := "HTTP/1.1 101 x\r\n\r\n"
	for _, limit := range []int64{0, -1, math.MinInt64, math.MaxInt64} {
		if _, _, err := readHandshakeResponse(strings.NewReader(head), limit); err != nil {
			t.Fatalf("limit=%d error=%v", limit, err)
		}
	}
	r := &responseHeadReader{r: strings.NewReader("x"), remaining: 0}
	if n, err := r.Read(nil); n != 0 || err != nil || r.exceeded {
		t.Fatalf("empty read=%d %v", n, err)
	}
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != ErrResponseHeaderTooLarge || !r.exceeded {
		t.Fatalf("exhausted read=%d %v", n, err)
	}
}

func FuzzResponseHeadBound(f *testing.F) {
	for _, head := range []string{"HTTP/1.1 101 x\r\nX: one\r\n\r\n", "HTTP/1.1 403 x\r\nContent-Length: 4\r\n\r\nbody", "bad\r\n\r\n"} {
		for _, n := range []uint16{1, 16, 32, 4096} {
			f.Add([]byte(head), n)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, budget uint16) {
		if len(data) > 8192 {
			t.Skip()
		}
		limit := int64(budget%4096) + 1
		src := &responseCountReader{r: bytes.NewReader(data)}
		resp, br, err := readHandshakeResponse(src, limit)
		if src.n > limit {
			t.Fatalf("read=%d budget=%d", src.n, limit)
		}
		if err != nil {
			if resp != nil {
				t.Fatal("partial response escaped")
			}
			return
		}
		plain := bufio.NewReader(bytes.NewReader(data))
		want, werr := http.ReadResponse(plain, nil)
		if werr != nil || resp.Status != want.Status {
			t.Fatalf("bounded=%v plain=%v error=%v", resp, want, werr)
		}
		gotTail, e1 := io.ReadAll(br)
		wantTail, e2 := io.ReadAll(plain)
		if !bytes.Equal(gotTail, wantTail) || !errors.Is(e1, e2) {
			t.Fatal("buffered tail changed")
		}
	})
}
