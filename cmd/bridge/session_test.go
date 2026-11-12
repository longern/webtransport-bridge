package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/longern/webtransport-bridge/internal/bridge"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

const testOrigin = "https://portal.example"

func TestSourceNetworkIgnoresPortsAndGroupsIPv6(t *testing.T) {
	for remote, want := range map[string]string{
		"192.0.2.5:1234":                 "192.0.2.5/32",
		"[::ffff:192.0.2.5]:5678":        "192.0.2.5/32",
		"[2001:db8:abcd:5::1]:1234":      "2001:db8:abcd:5::/64",
		"[2001:db8:abcd:5:ffff::2]:5678": "2001:db8:abcd:5::/64",
		"[fe80::1%en0]:4321":             "fe80::/64",
	} {
		got, err := sourceNetwork(remote)
		if err != nil || got.String() != want {
			t.Errorf("sourceNetwork(%q) = %v, %v; want %s", remote, got, err, want)
		}
	}
}

func TestWebTransportSessionAdmissionAndIdle(t *testing.T) {
	const idle = time.Second
	client, ipv4, ipv6 := startSessionTestServer(t, idle, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	})
	first := dialSession(t, client, ipv4, http.StatusOK)
	dialSession(t, client, ipv4, http.StatusOK)
	dialSession(t, client, ipv4, http.StatusTooManyRequests)
	dialSession(t, client, ipv6, http.StatusOK)
	dialSession(t, client, ipv6, http.StatusServiceUnavailable)

	// The connection's idle timeout is 10s and the server sends keepalives.
	// The application should close the session well before that.
	waitForIdleClose(t, first)
	replacement := dialSession(t, client, ipv4, http.StatusOK)
	checkResponse(t, openExchange(t, replacement, "/"), "ok")
}

func TestWebTransportExchangeAdmissionAndActiveSession(t *testing.T) {
	const idle = 750 * time.Millisecond
	started := make(chan struct{}, 2)
	unblock := make(chan struct{})
	client, ipv4, ipv6 := startSessionTestServer(t, idle, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blocked" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": connected\n\n")
			w.(http.Flusher).Flush()
			started <- struct{}{}
			select {
			case <-unblock:
				fmt.Fprint(w, "data: done\n\n")
			case <-r.Context().Done():
			}
			return
		}
		fmt.Fprint(w, "ok")
	})
	first := dialSession(t, client, ipv4, http.StatusOK)
	second := dialSession(t, client, ipv4, http.StatusOK)
	firstExchange := openExchange(t, first, "/blocked")
	secondExchange := openExchange(t, second, "/blocked")
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("exchange did not reach the backend")
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	excess, err := first.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = excess.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(excess, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	_ = excess.Close()
	_, err = excess.Read(make([]byte, 1))
	var reset *webtransport.StreamError
	if !errors.As(err, &reset) || reset.ErrorCode != 1 {
		t.Fatalf("over-limit exchange was not reset: %v", err)
	}
	other := dialSession(t, client, ipv6, http.StatusOK)
	checkResponse(t, openExchange(t, other, "/"), "ok")
	sse, err := http.ReadResponse(bufio.NewReader(firstExchange), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Body.Close()
	firstEvent := make([]byte, len(": connected\n\n"))
	if _, err = io.ReadFull(sse.Body, firstEvent); err != nil || string(firstEvent) != ": connected\n\n" {
		t.Fatalf("first SSE event = %q, %v", firstEvent, err)
	}

	select {
	case <-first.Context().Done():
		t.Fatal("session closed during active SSE")
	case <-time.After(idle + 100*time.Millisecond):
	}
	close(unblock)
	if body, err := io.ReadAll(sse.Body); err != nil || string(body) != "data: done\n\n" {
		t.Fatalf("final SSE event = %q, %v", body, err)
	}
	checkResponse(t, secondExchange, ": connected\n\ndata: done\n\n")
	waitForIdleClose(t, first)
}

func startSessionTestServer(t *testing.T, idle time.Duration, handler http.HandlerFunc) (*webtransport.Transport, string, string) {
	t.Helper()
	backend := httptest.NewTLSServer(handler)
	t.Cleanup(backend.Close)
	base := bridge.Bridge{
		Transport: backend.Client().Transport, MaxHeaderBytes: 32768, IdleTimeout: 5 * time.Second,
	}
	routes, err := legacyRoutes(base, backend.URL, testOrigin, "/", "GET", "", true)
	if err != nil {
		t.Fatal(err)
	}
	client, ipv4, ipv6 := startRoutedSessionTestServer(t, idle, backend, routes)
	return client, ipv4 + defaultSessionPath, ipv6 + defaultSessionPath
}

func startRoutedSessionTestServer(t *testing.T, idle time.Duration, tlsSource *httptest.Server, routes sessionRoutes) (*webtransport.Transport, string, string) {
	t.Helper()
	ipv4, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ipv4.Close() })
	ipv6, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ipv6.Close() })
	handler := &sessionHandler{
		routes: routes, idleTimeout: idle,
		sessions:  newConcurrencyLimit(3, 2),
		exchanges: newConcurrencyLimit(3, 2),
	}
	server := &webtransport.Server{
		H3: &http3.Server{
			Handler: handler,
			TLSConfig: &tls.Config{
				Certificates: tlsSource.TLS.Certificates,
				NextProtos:   []string{http3.NextProtoH3},
			},
			QUICConfig: &quic.Config{KeepAlivePeriod: 25 * time.Millisecond, MaxIdleTimeout: 10 * time.Second},
		},
		CheckOrigin: routes.checkOrigin,
	}
	handler.server = server
	for _, listener := range []net.PacketConn{ipv4, ipv6} {
		go func() { _ = server.Serve(listener) }()
	}
	t.Cleanup(func() { _ = server.Close() })
	tlsConfig := tlsSource.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.ServerName = "127.0.0.1"
	client := &webtransport.Transport{TLSClientConfig: tlsConfig}
	t.Cleanup(func() { _ = client.Close() })
	return client, "https://" + ipv4.LocalAddr().String(), "https://" + ipv6.LocalAddr().String()
}

func dialSession(t *testing.T, client *webtransport.Transport, endpoint string, want int) *webtransport.Session {
	t.Helper()
	headers := http.Header{
		"Origin":          {testOrigin},
		"X-Forwarded-For": {"192.0.2.1"},
	}
	if want == http.StatusTooManyRequests {
		headers.Set("X-Forwarded-For", "192.0.2.2")
	}
	return dialSessionWithHeaders(t, client, endpoint, headers, want)
}

func dialSessionWithHeaders(t *testing.T, client *webtransport.Transport, endpoint string, headers http.Header, want int) *webtransport.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	response, session, err := client.Dial(ctx, endpoint, headers)
	if response == nil || response.StatusCode != want {
		t.Fatalf("CONNECT %s = %v, %v; want %d", endpoint, response, err, want)
	}
	if want == http.StatusOK {
		if err != nil || session == nil {
			t.Fatalf("CONNECT did not open a session: %v", err)
		}
		t.Cleanup(func() { _ = session.CloseWithError(0, "") })
	} else if response.Body != nil {
		_ = response.Body.Close()
	}
	return session
}

func openExchange(t *testing.T, session *webtransport.Session, path string) *webtransport.Stream {
	t.Helper()
	return openRawExchange(t, session, fmt.Sprintf("GET %s HTTP/1.1\r\nHost: test\r\n\r\n", path))
}

func openRawExchange(t *testing.T, session *webtransport.Session, request string) *webtransport.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err = io.WriteString(stream, request); err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	return stream
}

func checkResponse(t *testing.T, stream *webtransport.Stream, want string) {
	t.Helper()
	checkExchange(t, stream, http.StatusOK, want)
}

func checkExchange(t *testing.T, stream *webtransport.Stream, status int, want string) {
	t.Helper()
	response, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if response.StatusCode != status || string(body) != want || err != nil {
		t.Fatalf("exchange = %d, %q, %v", response.StatusCode, body, err)
	}
}

func TestWebTransportRoutesKeepSessionBackendAndPolicy(t *testing.T) {
	photos := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "photos:"+r.URL.RequestURI())
	}))
	t.Cleanup(photos.Close)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("gateway token reached the backend")
		}
		fmt.Fprint(w, "files:"+r.URL.RequestURI())
	}))
	t.Cleanup(files.Close)
	config := fmt.Sprintf(`version: 1
backends:
  photos: %q
  files: %q
rules:
  - matches:
      - path: { type: Exact, value: /webtransport/photos }
        headers: [{ type: Exact, name: Origin, value: https://photos.example }]
      - path: { type: Exact, value: /webtransport/shared }
        headers: [{ type: Exact, name: origin, value: https://photos.example }]
    backendRefs: [{ name: photos }]
    streamPolicy:
      public: true
      allowedPaths: [/dav]
      allowedMethods: [GET]
  - matches:
      - path: { type: Exact, value: /webtransport/shared }
        headers: [{ type: Exact, name: Origin, value: https://files.example }]
    backendRefs: [{ name: files }]
    streamPolicy:
      tokenEnv: FILES_TOKEN
      allowedPaths: [/api]
      allowedMethods: [GET]
`, photos.URL, files.URL)
	base := bridge.Bridge{Transport: photos.Client().Transport, MaxHeaderBytes: 32768}
	routes, err := decodeRoutes(strings.NewReader(config), base, func(name string) string {
		if name == "FILES_TOKEN" {
			return "files-secret"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	client, endpoint, _ := startRoutedSessionTestServer(t, 5*time.Second, photos, routes)
	photoOrigin := http.Header{"Origin": {"https://photos.example"}}
	fileOrigin := http.Header{"Origin": {"https://files.example"}}
	dialSessionWithHeaders(t, client, endpoint+"/webtransport/photos", fileOrigin, http.StatusForbidden)
	dialSessionWithHeaders(t, client, endpoint+"/webtransport/shared", http.Header{}, http.StatusForbidden)
	dialSessionWithHeaders(t, client, endpoint+"/webtransport/shared", http.Header{
		"Origin": {"https://photos.example", "https://files.example"},
	}, http.StatusForbidden)
	for _, path := range []string{"/webtransport", "/webtransport/photos/extra", "/webtransport/%70hotos", "/webtransport/photos?route=files"} {
		dialSessionWithHeaders(t, client, endpoint+path, photoOrigin, http.StatusNotFound)
	}
	photoSession := dialSessionWithHeaders(t, client, endpoint+"/webtransport/shared", photoOrigin, http.StatusOK)
	fileSession := dialSessionWithHeaders(t, client, endpoint+"/webtransport/shared", fileOrigin, http.StatusOK)
	checkResponse(t, openExchange(t, photoSession, "/dav/example.jpg?download=1"), "photos:/dav/example.jpg?download=1")
	checkExchange(t, openExchange(t, photoSession, "/api/files"), http.StatusForbidden, "Forbidden\n")
	checkExchange(t, openRawExchange(t, photoSession, "POST /dav/example.jpg HTTP/1.1\r\nHost: test\r\n\r\n"), http.StatusMethodNotAllowed, "Method Not Allowed\n")
	checkExchange(t, openExchange(t, fileSession, "/api/files"), http.StatusProxyAuthRequired, "Proxy Authentication Required\n")
	// Inner Host and Origin cannot switch the already admitted session's backend.
	checkResponse(t, openRawExchange(t, fileSession, "GET /api/files HTTP/1.1\r\nHost: photos.example\r\nOrigin: https://photos.example\r\nProxy-Authorization: Bearer files-secret\r\n\r\n"), "files:/api/files")
	checkResponse(t, openExchange(t, photoSession, "/dav/second.jpg"), "photos:/dav/second.jpg")
}

func waitForIdleClose(t *testing.T, session *webtransport.Session) {
	t.Helper()
	select {
	case <-session.Context().Done():
		_, err := session.AcceptStream(t.Context())
		var closed *webtransport.SessionError
		if !errors.As(err, &closed) || !closed.Remote || closed.ErrorCode != 0 {
			t.Fatalf("expected a graceful peer close, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle session remained open")
	}
}
