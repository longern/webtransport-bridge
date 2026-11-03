package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/longern/webtransport-bridge/internal/bridge"
	"github.com/longern/webtransport-bridge/internal/certmanager"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

func main() {
	addr := flag.String("listen", ":4433", "UDP listen address")
	backend := flag.String("backend", "", "fixed backend origin, e.g. http://caddy:80")
	cert := flag.String("cert", "/certs/cert.pem", "TLS certificate PEM")
	key := flag.String("key", "/certs/key.pem", "TLS key PEM")
	origins := flag.String("origins", "", "comma separated exact HTTPS page origins")
	prefixes := flag.String("paths", "/", "comma separated permitted path prefixes")
	methods := flag.String("methods", "GET,HEAD,POST,PUT", "permitted methods; add WebDAV methods explicitly")
	public := flag.Bool("public", false, "explicitly allow requests without a Bearer token")
	timeout := flag.Duration("timeout", 5*time.Minute, "total time per exchange")
	maxBody := flag.Int64("max-body", 1<<30, "maximum request body bytes")
	maxHeaders := flag.Int("max-headers", 32<<10, "maximum HTTP header bytes")
	maxStreams := flag.Int("max-streams", 32, "global concurrent HTTP exchanges")
	maxSessions := flag.Int("max-sessions", 128, "maximum active sessions")
	autoCert := flag.Bool("auto-cert", false, "automatically generate and rotate certificates; requires CERT_WEBHOOK_URL")
	certHost := flag.String("cert-host", "", "optional hostname or IP for automatic certificates (default label: webtransport-bridge.invalid)")
	certDir := flag.String("cert-dir", "/certs", "persistent directory for automatic certificate state")
	publishDelay := flag.Duration("cert-publish-delay", time.Hour, "delay between webhook confirmation and certificate activation")
	flag.Parse()
	u, e := url.Parse(*backend)
	if e != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		log.Fatal("-backend must be an HTTP(S) origin without credentials, path or query")
	}
	token := os.Getenv("BRIDGE_TOKEN")
	if token == "" && !*public {
		log.Fatal("set BRIDGE_TOKEN or explicitly use -public")
	}
	if *origins == "" || *timeout <= 0 || *maxBody < 0 || *maxHeaders < 1024 || *maxStreams < 1 || *maxSessions < 1 {
		log.Fatal("origins and positive limits are required")
	}
	allowed := map[string]bool{}
	for _, o := range strings.Split(*origins, ",") {
		o = strings.TrimSpace(o)
		v, err := url.Parse(o)
		if err != nil || v.Scheme != "https" || v.Host == "" || v.Path != "" || v.RawQuery != "" || v.Fragment != "" || v.User != nil {
			log.Fatal("origins must be exact HTTPS origins without trailing slash")
		}
		allowed[o] = true
	}
	mm := map[string]bool{}
	for _, m := range strings.Split(*methods, ",") {
		mm[strings.TrimSpace(m)] = true
	}
	for _, p := range strings.Split(*prefixes, ",") {
		if !strings.HasPrefix(p, "/") {
			log.Fatal("path prefixes must start with /")
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableCompression = true
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.MaxResponseHeaderBytes = int64(*maxHeaders)
	defer tr.CloseIdleConnections()
	b := &bridge.Bridge{Backend: u, Token: token, Prefixes: strings.Split(*prefixes, ","), Methods: mm, Transport: tr, Timeout: *timeout, MaxBodyBytes: *maxBody, MaxHeaderBytes: *maxHeaders}
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
	mux := http.NewServeMux()
	s := &webtransport.Server{H3: &http3.Server{Addr: *addr, Handler: mux, TLSConfig: tlsConfig}, Config: &webtransport.Config{MaxIncomingStreams: int64(*maxStreams), MaxIncomingUniStreams: -1}, CheckOrigin: func(r *http.Request) bool { return allowed[r.Header.Get("Origin")] }}
	slots := make(chan struct{}, *maxStreams)
	sessions := make(chan struct{}, *maxSessions)
	mux.HandleFunc("/webtransport", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/webtransport" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		select {
		case sessions <- struct{}{}:
			defer func() { <-sessions }()
		default:
			http.Error(w, "session limit", 503)
			return
		}
		session, err := s.Upgrade(w, r)
		if err != nil {
			return
		}
		defer session.CloseWithError(0, "")
		for {
			stream, err := session.AcceptStream(session.Context())
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				go func() {
					defer func() { <-slots }()
					defer stream.CancelRead(0)
					defer stream.Close()
					if err := b.Handle(stream.Context(), stream); err != nil {
						stream.CancelWrite(1)
						log.Printf("exchange failed: %v", err)
					}
				}()
			default:
				stream.CancelRead(1)
				stream.CancelWrite(1)
			}
		}
	})
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
