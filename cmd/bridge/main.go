package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/longern/webtransport-bridge/internal/bridge"
	"github.com/longern/webtransport-bridge/internal/certmanager"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

func main() {
	addr := flag.String("listen", ":4433", "UDP listen address")
	config := flag.String("config", "", "YAML file for routing WebTransport sessions to multiple backends")
	backend := flag.String("backend", "", "fixed backend origin, e.g. http://caddy:80")
	cert := flag.String("cert", "/certs/cert.pem", "TLS certificate PEM")
	key := flag.String("key", "/certs/key.pem", "TLS key PEM")
	origins := flag.String("origins", "", "comma separated exact HTTPS page origins")
	prefixes := flag.String("paths", "/", "comma separated permitted path prefixes")
	methods := flag.String("methods", defaultMethods, "permitted methods, or * for all supported methods")
	public := flag.Bool("public", false, "disable gateway token checks and delegate authentication to the backend")
	timeout := flag.Duration("timeout", 0, "optional total exchange deadline; 0 disables it")
	idleTimeout := flag.Duration("idle-timeout", time.Minute, "exchange timeout without request or response byte progress")
	headerTimeout := flag.Duration("header-timeout", 15*time.Second, "absolute deadline for receiving request headers")
	responseHeaderTimeout := flag.Duration("response-header-timeout", 30*time.Second, "backend response header deadline after uploading the request")
	keepAlive := flag.Duration("keepalive", 15*time.Second, "QUIC keepalive interval; 0 disables it")
	connectionIdle := flag.Duration("connection-idle-timeout", time.Minute, "QUIC connection idle timeout")
	sessionIdle := flag.Duration("session-idle-timeout", time.Minute, "close a WebTransport session after this long without an active exchange")
	maxBody := flag.Int64("max-body", 1<<30, "maximum request body bytes")
	maxHeaders := flag.Int("max-headers", 32<<10, "maximum HTTP header bytes")
	maxStreams := flag.Int("max-streams", 32, "global concurrent HTTP exchanges")
	maxSessions := flag.Int("max-sessions", 128, "maximum active sessions")
	maxStreamsPerIP := flag.Int("max-streams-per-ip", 16, "concurrent HTTP exchanges per IPv4 address or IPv6 /64")
	maxSessionsPerIP := flag.Int("max-sessions-per-ip", 16, "active sessions per IPv4 address or IPv6 /64")
	autoCert := flag.Bool("auto-cert", false, "automatically generate and rotate certificates; requires CERT_WEBHOOK_URL")
	certHost := flag.String("cert-host", "", "optional hostname or IP for automatic certificates (default label: webtransport-bridge.invalid)")
	certDir := flag.String("cert-dir", "/certs", "persistent directory for automatic certificate state")
	publishDelay := flag.Duration("cert-publish-delay", time.Hour, "delay between webhook confirmation and certificate activation")
	flag.Parse()
	if *timeout < 0 || *idleTimeout <= 0 || *headerTimeout <= 0 || *responseHeaderTimeout <= 0 || *keepAlive < 0 || *connectionIdle <= 0 || *sessionIdle <= 0 ||
		*maxBody < 0 || *maxHeaders < 1024 || *maxStreams < 1 || *maxSessions < 1 || *maxStreamsPerIP < 1 || *maxSessionsPerIP < 1 {
		log.Fatal("invalid timeouts or limits")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableCompression = true
	tr.ResponseHeaderTimeout = *responseHeaderTimeout
	tr.MaxResponseHeaderBytes = int64(*maxHeaders)
	defer tr.CloseIdleConnections()
	base := bridge.Bridge{
		Transport: tr, Timeout: *timeout, IdleTimeout: *idleTimeout,
		HeaderTimeout: *headerTimeout, MaxBodyBytes: *maxBody, MaxHeaderBytes: *maxHeaders,
	}
	var routes sessionRoutes
	var err error
	if *config != "" {
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "backend", "origins", "paths", "methods", "public":
				log.Fatalf("--%s cannot be combined with --config", f.Name)
			}
		})
		routes, err = readRoutes(*config, base)
	} else {
		routes, err = legacyRoutes(base, *backend, *origins, *prefixes, *methods, os.Getenv(defaultTokenEnv), *public)
	}
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{http3.NextProtoH3}}
	if *autoCert {
		manager, err := certmanager.New(certmanager.Options{Host: *certHost, Directory: *certDir, WebhookURL: os.Getenv("CERT_WEBHOOK_URL"), PublishDelay: *publishDelay})
		if err != nil {
			log.Fatal(err)
		}
		tlsConfig.GetCertificate = manager.GetCertificate
		go manager.Run(ctx)
	}
	handler := &sessionHandler{
		routes: routes, idleTimeout: *sessionIdle,
		sessions:  newConcurrencyLimit(*maxSessions, *maxSessionsPerIP),
		exchanges: newConcurrencyLimit(*maxStreams, *maxStreamsPerIP),
	}
	s := &webtransport.Server{
		H3: &http3.Server{Addr: *addr, Handler: handler, TLSConfig: tlsConfig, QUICConfig: &quic.Config{KeepAlivePeriod: *keepAlive, MaxIdleTimeout: *connectionIdle}},
		Config: &webtransport.Config{
			MaxIncomingStreams:    int64(*maxStreams),
			MaxIncomingUniStreams: -1,
			// Safari needs receive credit before it can send the first request.
			MaxIncomingData: 1 << 20,
		},
		CheckOrigin: routes.checkOrigin,
	}
	handler.server = s
	go func() { <-ctx.Done(); _ = s.Close() }()
	fmt.Printf("WebTransport HTTP bridge listening on UDP %s\n", *addr)
	var serveErr error
	if *autoCert {
		serveErr = s.ListenAndServe()
	} else {
		serveErr = s.ListenAndServeTLS(*cert, *key)
	}
	if serveErr != nil && ctx.Err() == nil {
		log.Fatal(serveErr)
	}
}

// Public mode explicitly delegates authentication, even if an old token remains
// in the environment. Without that explicit choice, missing credentials fail closed.
func configuredToken(token string, public bool) (string, error) {
	if public {
		return "", nil
	}
	if token == "" {
		return "", fmt.Errorf("set BRIDGE_TOKEN or explicitly use --public to delegate authentication to the backend")
	}
	return token, nil
}
