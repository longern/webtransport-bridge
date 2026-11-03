package bridge

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type memoryStream struct {
	io.Reader
	bytes.Buffer
}

func (s *memoryStream) Read(p []byte) (int, error)  { return s.Reader.Read(p) }
func (s *memoryStream) SetDeadline(time.Time) error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixture() *Bridge {
	u, _ := url.Parse("http://nas:8080")
	return &Bridge{Backend: u, Token: "secret", Prefixes: []string{"/public"}, Methods: map[string]bool{"GET": true, "HEAD": true, "PUT": true}, Timeout: time.Second, MaxHeaderBytes: 1024, MaxBodyBytes: 8}
}
func exchange(t *testing.T, b *Bridge, raw string) *http.Response {
	t.Helper()
	s := &memoryStream{Reader: strings.NewReader(raw)}
	if err := b.Handle(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	r, err := http.ReadResponse(bufio.NewReader(&s.Buffer), nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestForwardRange(t *testing.T) {
	b := fixture()
	b.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "http://nas:8080/public/a?q=1" || r.Host != "nas:8080" || r.Header.Get("Range") != "bytes=1-2" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Hop") != "" {
			t.Fatalf("unexpected forwarded request: %#v", r)
		}
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": []string{"bytes 1-2/3"}}, Body: io.NopCloser(strings.NewReader("bc")), ContentLength: 2}, nil
	})
	r := exchange(t, b, "GET /public/a?q=1 HTTP/1.1\r\nHost: anything\r\nAuthorization: Bearer secret\r\nRange: bytes=1-2\r\nConnection: X-Hop\r\nX-Hop: remove\r\n\r\n")
	data, _ := io.ReadAll(r.Body)
	if r.StatusCode != 206 || string(data) != "bc" || r.Header.Get("Content-Range") == "" {
		t.Fatal(r, string(data))
	}
}
func TestReject(t *testing.T) {
	for _, tt := range []struct {
		target, auth, extra string
		code                int
	}{
		{"/public/a", "", "", 401}, {"/private", "Bearer secret", "", 403},
		{"http://evil/public", "Bearer secret", "", 400}, {"/public/../private", "Bearer secret", "", 403},
		{"/public/%252e%252e/private", "Bearer secret", "", 403}, {"/public/a", "Bearer secret", "Content-Length: 9\r\n", 413},
		{"/public/a", "Bearer secret", "Expect: 100-continue\r\n", 417},
		{"/public/a", "Bearer secret", "X-Large: " + strings.Repeat("x", 1100) + "\r\n", 400},
	} {
		t.Run(tt.target+tt.extra, func(t *testing.T) {
			b := fixture()
			b.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("rejected request reached backend")
				return nil, nil
			})
			r := exchange(t, b, "GET "+tt.target+" HTTP/1.1\r\nHost: nas\r\nAuthorization: "+tt.auth+"\r\n"+tt.extra+"\r\n")
			if r.StatusCode != tt.code {
				t.Fatalf("got %d want %d", r.StatusCode, tt.code)
			}
		})
	}
}
func TestChunkedUpload(t *testing.T) {
	b := fixture()
	b.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if string(data) != "abc" {
			t.Fatal(string(data))
		}
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
	})
	r := exchange(t, b, "PUT /public/a HTTP/1.1\r\nHost: nas\r\nAuthorization: Bearer secret\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n")
	if r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	r = exchange(t, b, "PUT /public/a HTTP/1.1\r\nHost: nas\r\nAuthorization: Bearer secret\r\nTransfer-Encoding: chunked\r\n\r\n9\r\n123456789\r\n0\r\n\r\n")
	if r.StatusCode != 413 {
		t.Fatal(r.StatusCode)
	}
}
func TestHead(t *testing.T) {
	b := fixture()
	b.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody, ContentLength: 100}, nil
	})
	s := &memoryStream{Reader: strings.NewReader("HEAD /public/a HTTP/1.1\r\nHost: nas\r\nAuthorization: Bearer secret\r\n\r\n")}
	if err := b.Handle(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Buffer.String(), "Content-Length: 100\r\n") || !strings.HasSuffix(s.Buffer.String(), "\r\n\r\n") {
		t.Fatal(s.Buffer.String())
	}
}
