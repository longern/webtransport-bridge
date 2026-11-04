package bridge

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestBackendCredentialsSurviveBothGatewayModes(t *testing.T) {
	for _, auth := range []string{"Basic dXNlcjpwYXNz", "Bearer backend-secret", ""} {
		for _, protected := range []bool{false, true} {
			t.Run(auth+map[bool]string{true: "/protected", false: "/backend-only"}[protected], func(t *testing.T) {
				b := fixture()
				b.Token = ""
				gateway := "unneeded-gateway-secret"
				if protected {
					b.Token = "gateway-secret"
					gateway = b.Token
				}
				b.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("Authorization") != auth || r.Header.Get("Cookie") != "session=backend-cookie" {
						t.Fatalf("backend credentials changed: %v", r.Header)
					}
					if len(r.Header.Values("Proxy-Authorization")) != 0 {
						t.Fatal("gateway token leaked to backend")
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
				})
				r := exchange(t, b, "GET /public/a HTTP/1.1\r\nHost: nas\r\nAuthorization: "+auth+"\r\nCookie: session=backend-cookie\r\nProxy-Authorization: Bearer "+gateway+"\r\n\r\n")
				if r.StatusCode != 200 {
					t.Fatal(r.StatusCode)
				}
			})
		}
	}
}

func TestGatewayTokenCannotBeReplacedByBackendAuthorization(t *testing.T) {
	for _, headers := range []string{
		"Authorization: Bearer secret\r\n",
		"Authorization: Basic dXNlcjpwYXNz\r\nProxy-Authorization: Bearer wrong\r\n",
		"Proxy-Authorization: Bearer secret\r\nProxy-Authorization: Bearer other\r\n",
	} {
		t.Run(headers, func(t *testing.T) {
			b := fixture()
			b.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid gateway credential reached backend")
				return nil, nil
			})
			r := exchange(t, b, "GET /public/a HTTP/1.1\r\nHost: nas\r\n"+headers+"\r\n")
			if r.StatusCode != 407 || r.Header.Get("Proxy-Authenticate") != `Bearer realm="webtransport-bridge"` {
				t.Fatal(r)
			}
		})
	}
}

func TestBackendAuthenticationExchangeThroughHTTPTransport(t *testing.T) {
	for _, authorization := range []string{"Basic dXNlcjpwYXNz", "Bearer backend-secret", ""} {
		t.Run(authorization, func(t *testing.T) {
			b := streamingFixture()
			b.Token = ""
			captured := make(chan *http.Request, 1)
			transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() {
					defer server.Close()
					req, err := http.ReadRequest(bufio.NewReader(server))
					if err != nil {
						return
					}
					captured <- req
					_, _ = io.WriteString(server, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nWWW-Authenticate: Basic realm=\"backend\"\r\nSet-Cookie: session=one; HttpOnly\r\nSet-Cookie: csrf=two\r\n\r\n")
				}()
				return client, nil
			}}
			defer transport.CloseIdleConnections()
			b.Transport = transport
			client, result := streamExchange(t, b, context.Background())
			raw := "GET /public/a HTTP/1.1\r\nHost: nas\r\nCookie: session=existing\r\nProxy-Authorization: Bearer must-not-leak\r\n"
			if authorization != "" {
				raw += "Authorization: " + authorization + "\r\n"
			}
			if _, err := io.WriteString(client, raw+"\r\n"); err != nil {
				t.Fatal(err)
			}
			r, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") != `Basic realm="backend"` || len(r.Header.Values("Set-Cookie")) != 2 {
				t.Fatalf("backend challenge/cookies changed: %v", r)
			}
			req := <-captured
			if req.Header.Get("Authorization") != authorization || req.Header.Get("Cookie") != "session=existing" || req.Header.Get("Proxy-Authorization") != "" {
				t.Fatalf("backend request changed: %v", req.Header)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			waitExchange(t, result, false)
		})
	}
}

func TestPublicModeStripsAllGatewayTokenValues(t *testing.T) {
	b := fixture()
	b.Token = ""
	b.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Join(r.Header.Values("Proxy-Authorization"), "") != "" {
			t.Fatal("gateway token leaked")
		}
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
	})
	r := exchange(t, b, "GET /public/a HTTP/1.1\r\nHost: nas\r\nProxy-Authorization: Bearer first\r\nProxy-Authorization: Bearer second\r\n\r\n")
	if r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
}

func TestProxyBearerSchemeAndBackendAuthorizationAreIndependent(t *testing.T) {
	for _, credential := range []string{"bearer secret", "BEARER secret", "Bearer   secret"} {
		b := fixture()
		b.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Basic dXNlcjpwYXNz" || r.Header.Get("Proxy-Authorization") != "" {
				t.Fatal(r.Header)
			}
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
		})
		r := exchange(t, b, "GET /public/a HTTP/1.1\r\nHost: nas\r\nAuthorization: Basic dXNlcjpwYXNz\r\nProxy-Authorization: "+credential+"\r\n\r\n")
		if r.StatusCode != 204 {
			t.Fatal(r.StatusCode)
		}
	}
}
func TestMethodNotAllowedListsEnabledMethods(t *testing.T) {
	b := fixture()
	b.Methods["DELETE"] = false
	r := exchange(t, b, "DELETE /public/a HTTP/1.1\r\nHost: nas\r\nProxy-Authorization: Bearer secret\r\n\r\n")
	if r.StatusCode != 405 || r.Header.Get("Allow") != "GET, HEAD, PUT" {
		t.Fatal(r)
	}
}
