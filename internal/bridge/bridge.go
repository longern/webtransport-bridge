// Package bridge forwards one HTTP/1.1 exchange per bidirectional stream.
package bridge

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type Stream interface {
	io.Reader
	io.Writer
	SetDeadline(time.Time) error
}
type Bridge struct {
	Backend        *url.URL
	Token          string
	Prefixes       []string
	Methods        map[string]bool
	Transport      http.RoundTripper
	Timeout        time.Duration // Optional total deadline; zero disables it.
	IdleTimeout    time.Duration
	HeaderTimeout  time.Duration
	MaxHeaderBytes int
	MaxBodyBytes   int64
}

// headerReader limits only the header block, without buffering a body in memory.
type headerReader struct {
	r                 io.Reader
	n, limit, matched int
	done              bool
}

func (r *headerReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.done {
		return r.r.Read(p)
	}
	if r.n >= r.limit {
		return 0, errors.New("header limit exceeded")
	}
	var b [1]byte
	n, e := r.r.Read(b[:])
	if n == 0 {
		return n, e
	}
	r.n++
	p[0] = b[0]
	const end = "\r\n\r\n"
	if b[0] == end[r.matched] {
		r.matched++
	} else if b[0] == '\r' {
		r.matched = 1
	} else {
		r.matched = 0
	}
	r.done = r.matched == 4
	return 1, e
}

func strip(h http.Header) {
	for _, line := range h.Values("Connection") {
		for _, key := range strings.Split(line, ",") {
			h.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}
func (b *Bridge) allowed(p string) bool {
	// Reject ambiguous paths instead of forwarding a path the backend might normalize differently.
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != strings.TrimSuffix(p, "/") && p != "/" {
		return false
	}
	for _, prefix := range b.Prefixes {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}
func reply(w io.Writer, code int, head bool) error {
	text := http.StatusText(code) + "\n"
	h := http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}}
	if code == 401 {
		h.Set("WWW-Authenticate", `Bearer realm="webtransport-bridge"`)
	}
	r := &http.Response{StatusCode: code, ProtoMajor: 1, ProtoMinor: 1, Header: h, Body: io.NopCloser(strings.NewReader(text)), ContentLength: int64(len(text)), Close: true}
	if head {
		r.Request = &http.Request{Method: "HEAD"}
	}
	return r.Write(w)
}

// Handle never buffers the complete upload or download. Caller owns stream closure.
func (b *Bridge) Handle(ctx context.Context, s Stream) error {
	idle, header := b.IdleTimeout, b.HeaderTimeout
	if idle == 0 {
		idle = time.Minute
	}
	if header == 0 {
		header = 15 * time.Second
	}
	watch, ctx := watchExchange(ctx, s, idle, header, b.Timeout)
	var responseBody io.ReadCloser
	defer func() {
		watch.stop() // Cancel blocked upstream operations before closing their body.
		if responseBody != nil {
			_ = responseBody.Close()
		}
	}()
	req, err := http.ReadRequest(bufio.NewReader(&headerReader{r: watch, limit: b.MaxHeaderBytes}))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return reply(watch, 400, false)
	}
	watch.headersDone()
	// Do not drain untrusted bodies on rejection. The stream owner stops receiving.
	head := req.Method == "HEAD"
	if req.ProtoMajor != 1 || req.ProtoMinor != 1 || !strings.HasPrefix(req.RequestURI, "/") || strings.HasPrefix(req.RequestURI, "//") || req.URL.IsAbs() {
		return reply(watch, 400, head)
	}
	if b.Token != "" && subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+b.Token)) != 1 {
		return reply(watch, 401, head)
	}
	if !b.Methods[req.Method] {
		return reply(watch, 405, head)
	}
	// Multiple decoding by an upstream must not allow escaping the path scope.
	if strings.Contains(req.URL.Path, "%") || !b.allowed(req.URL.Path) {
		return reply(watch, 403, head)
	}
	if req.Header.Get("Expect") != "" || req.Header.Get("Upgrade") != "" {
		return reply(watch, 417, head)
	}
	if req.ContentLength > b.MaxBodyBytes {
		return reply(watch, 413, head)
	}
	req.Body = http.MaxBytesReader(nil, io.NopCloser(req.Body), b.MaxBodyBytes)
	// The stream owner stops receiving on return. Avoid http.ReadRequest's
	// draining Close: a rejected or failed upload must not keep reading forever.
	defer req.Body.Close()
	req = req.WithContext(ctx)
	req.RequestURI = ""
	req.URL.Scheme = b.Backend.Scheme
	req.URL.Host = b.Backend.Host
	req.Host = b.Backend.Host
	req.Header.Del("Authorization")
	strip(req.Header)
	// Avoid transparent decompression so Content-Encoding and bytes stay consistent.
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	req.Close = false
	transport := b.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		var large *http.MaxBytesError
		var timedOut interface{ Timeout() bool }
		if ctx.Err() != nil {
			return ctx.Err()
		}
		code := 502
		if errors.As(err, &large) {
			code = 413
		} else if errors.As(err, &timedOut) && timedOut.Timeout() {
			code = 504
		}
		return reply(watch, code, head)
	}
	responseBody = resp.Body
	if resp.StatusCode == 101 {
		return reply(watch, 502, head)
	}
	strip(resp.Header)
	resp.Trailer = nil
	resp.ProtoMajor = 1
	resp.ProtoMinor = 1
	resp.Close = true
	resp.Request = req
	// Write directly to the stream: wrapping it in a buffered writer would
	// delay small streaming responses such as SSE events.
	return resp.Write(watch)
}
