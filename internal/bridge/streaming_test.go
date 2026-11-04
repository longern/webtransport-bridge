package bridge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

const requestHeaders = "GET /public/events HTTP/1.1\r\nHost: nas\r\nAuthorization: Bearer secret\r\n\r\n"

func streamExchange(t *testing.T, b *Bridge, ctx context.Context) (net.Conn, <-chan error) {
	t.Helper()
	client, server := net.Pipe()
	result := make(chan error, 1)
	go func() { defer server.Close(); result <- b.Handle(ctx, server) }()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, result
}
func waitExchange(t *testing.T, result <-chan error, wantError bool) {
	t.Helper()
	select {
	case err := <-result:
		if (err != nil) != wantError {
			t.Fatalf("exchange error = %v; want error: %v", err, wantError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not finish")
	}
}
func streamingFixture() *Bridge {
	b := fixture()
	b.Timeout = 0
	b.IdleTimeout = 300 * time.Millisecond
	b.HeaderTimeout = time.Second
	return b
}

func TestStreamingResponseArrivesBeforeEOFAndContinuesPastIdleWindow(t *testing.T) {
	b := streamingFixture()
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close(); writer.Close() })
	b.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader, ContentLength: -1, TransferEncoding: []string{"chunked"}}, nil
	})
	client, result := streamExchange(t, b, context.Background())
	if _, err := io.WriteString(client, requestHeaders); err != nil {
		t.Fatal(err)
	}
	// Headers must arrive even before the backend emits its first event.
	resp, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 7; i++ {
		const event = "data: hello\n\n"
		sent := make(chan error, 1)
		go func() { _, err := io.WriteString(writer, event); sent <- err }()
		data := make([]byte, len(event))
		if _, err := io.ReadFull(resp.Body, data); err != nil {
			t.Fatal(err)
		}
		if string(data) != event {
			t.Fatal(string(data))
		}
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			t.Fatalf("response ended before backend EOF: %v", err)
		default:
		}
		time.Sleep(70 * time.Millisecond)
	}
	if time.Since(start) <= b.IdleTimeout {
		t.Fatal("test did not exceed an idle window")
	}
	writer.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	waitExchange(t, result, false)
}

func TestActiveUploadCanOutliveIdleWindow(t *testing.T) {
	b := streamingFixture()
	b.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if string(data) != "1234567" {
			return nil, errors.New("incorrect upload")
		}
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
	})
	client, result := streamExchange(t, b, context.Background())
	headers := "PUT /public/a HTTP/1.1\r\nHost: nas\r\nAuthorization: Bearer secret\r\nContent-Length: 7\r\n\r\n"
	if _, err := io.WriteString(client, headers); err != nil {
		t.Fatal(err)
	}
	for _, c := range "1234567" {
		time.Sleep(70 * time.Millisecond)
		if _, err := io.WriteString(client, string(c)); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	waitExchange(t, result, false)
}

func TestIdleTimeoutCancelsBackendWaitingForHeaders(t *testing.T) {
	b := streamingFixture()
	b.IdleTimeout = 100 * time.Millisecond
	cancelled := make(chan struct{})
	b.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		close(cancelled)
		return nil, req.Context().Err()
	})
	client, result := streamExchange(t, b, context.Background())
	if _, err := io.WriteString(client, requestHeaders); err != nil {
		t.Fatal(err)
	}
	waitExchange(t, result, true)
	select {
	case <-cancelled:
	default:
		t.Fatal("backend was not cancelled")
	}
}

func TestIdleTimeoutUnblocksUploadAndResponseBody(t *testing.T) {
	for _, upload := range []bool{true, false} {
		t.Run(map[bool]string{true: "upload", false: "response-body"}[upload], func(t *testing.T) {
			b := streamingFixture()
			b.IdleTimeout = 100 * time.Millisecond
			b.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if upload {
					_, err := io.ReadAll(req.Body)
					return nil, err
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &contextBody{ctx: req.Context()}, ContentLength: -1}, nil
			})
			client, result := streamExchange(t, b, context.Background())
			raw := requestHeaders
			if upload {
				raw = "PUT /public/a HTTP/1.1\r\nHost: nas\r\nAuthorization: Bearer secret\r\nContent-Length: 8\r\n\r\n12"
			}
			if _, err := io.WriteString(client, raw); err != nil {
				t.Fatal(err)
			}
			if !upload {
				if _, err := http.ReadResponse(bufio.NewReader(client), nil); err != nil {
					t.Fatal(err)
				}
			}
			waitExchange(t, result, true)
		})
	}
}

type contextBody struct{ ctx context.Context }

func (b *contextBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *contextBody) Close() error             { return nil }

func TestIdleTimeoutUnblocksSlowResponseConsumer(t *testing.T) {
	b := streamingFixture()
	b.IdleTimeout = 100 * time.Millisecond
	b.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("abc")), ContentLength: 3}, nil
	})
	client, result := streamExchange(t, b, context.Background())
	if _, err := io.WriteString(client, requestHeaders); err != nil {
		t.Fatal(err)
	}
	// Do not read: a blocked downstream Write must also be interrupted.
	waitExchange(t, result, true)
}

func TestHeaderDeadlineDoesNotSlideWithTrickleTraffic(t *testing.T) {
	b := streamingFixture()
	b.HeaderTimeout = 100 * time.Millisecond
	client, result := streamExchange(t, b, context.Background())
	go func() {
		for i := 0; i < 30; i++ {
			if _, err := io.WriteString(client, "G"); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	waitExchange(t, result, true)
}

func TestClientCancellationInterruptsBackendAndStream(t *testing.T) {
	for _, inHeaders := range []bool{true, false} {
		t.Run(map[bool]string{true: "request-headers", false: "backend"}[inHeaders], func(t *testing.T) {
			b := streamingFixture()
			b.IdleTimeout = 10 * time.Second
			started := make(chan struct{})
			b.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-req.Context().Done()
				return nil, req.Context().Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client, result := streamExchange(t, b, ctx)
			if !inHeaders {
				if _, err := io.WriteString(client, requestHeaders); err != nil {
					t.Fatal(err)
				}
				<-started
			}
			cancel()
			waitExchange(t, result, true)
		})
	}
}

func TestOptionalTotalDeadlineStillLimitsActiveTransfer(t *testing.T) {
	b := streamingFixture()
	b.Timeout = 100 * time.Millisecond
	b.IdleTimeout = time.Second
	b.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &contextBody{ctx: req.Context()}, ContentLength: -1}, nil
	})
	client, result := streamExchange(t, b, context.Background())
	if _, err := io.WriteString(client, requestHeaders); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(client), nil); err != nil {
		t.Fatal(err)
	}
	waitExchange(t, result, true)
}

// Exercise net/http's real transport and chunk decoder without opening sockets.
func TestHTTPTransportStreamsSSEAndCancelsBackend(t *testing.T) {
	b := streamingFixture()
	b.IdleTimeout = 2 * time.Second
	emit := make(chan struct{})
	backendDone := make(chan struct{})
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer close(backendDone)
			defer server.Close()
			if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
				return
			}
			if _, err := io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n"); err != nil {
				return
			}
			<-emit
			if _, err := io.WriteString(server, "9\r\ndata: x\n\n\r\n"); err != nil {
				return
			}
			// Wait for the transport to close its connection after client cancellation.
			_, _ = io.Copy(io.Discard, server)
		}()
		return client, nil
	}, DisableCompression: true}
	defer transport.CloseIdleConnections()
	b.Transport = transport
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, result := streamExchange(t, b, ctx)
	if _, err := io.WriteString(client, requestHeaders); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	close(emit)
	event := make([]byte, 9)
	if _, err := io.ReadFull(resp.Body, event); err != nil {
		t.Fatal(err)
	}
	if string(event) != "data: x\n\n" {
		t.Fatal(string(event))
	}
	cancel()
	waitExchange(t, result, true)
	select {
	case <-backendDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream connection was not closed")
	}
}

func TestBackendResponseHeaderTimeoutReturns504(t *testing.T) {
	b := streamingFixture()
	b.IdleTimeout = 2 * time.Second
	backendDone := make(chan struct{})
	transport := &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond, DialContext: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { defer server.Close(); defer close(backendDone); _, _ = io.Copy(io.Discard, server) }()
		return client, nil
	}}
	defer transport.CloseIdleConnections()
	b.Transport = transport
	client, result := streamExchange(t, b, context.Background())
	if _, err := io.WriteString(client, requestHeaders); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 504 {
		t.Fatal(resp.StatusCode)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	waitExchange(t, result, false)
	select {
	case <-backendDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream connection was not closed")
	}
}
