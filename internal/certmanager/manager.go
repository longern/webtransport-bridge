// Package certmanager manages short-lived certificates and publishes their pins.
package certmanager

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const lifetime = 13 * 24 * time.Hour
const renewBefore = 48 * time.Hour

type Options struct {
	Host, Directory, WebhookURL string
	PublishDelay                time.Duration
	Client                      *http.Client
}
type state struct {
	Active string `json:"active"`
	Next   string `json:"next,omitempty"`
}
type Pin struct {
	Algorithm string    `json:"algorithm"`
	Value     string    `json:"value"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
}
type Payload struct {
	Version      int   `json:"version"`
	Certificates []Pin `json:"certificates"`
}
type Manager struct {
	opts        Options
	state       state
	current     atomic.Value
	announcedAt time.Time
	published   string
}

func New(o Options) (*Manager, error) {
	u, err := url.Parse(o.WebhookURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("certificate webhook must be an HTTPS URL without user info or fragment")
	}
	if o.Host == "" || o.Directory == "" || o.PublishDelay < time.Minute || o.PublishDelay > 24*time.Hour {
		return nil, errors.New("certificate host, directory and publish delay between 1m and 24h are required")
	}
	if err := os.MkdirAll(o.Directory, 0700); err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 15 * time.Second}
	if o.Client != nil {
		client = *o.Client
	}
	// The URL is a credential: never send it to a redirect target or print errors containing it.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 15 * time.Second
	o.Client = &client
	m := &Manager{opts: o}
	data, err := os.ReadFile(filepath.Join(o.Directory, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		m.state.Active, err = generate(o.Host, time.Now())
		if err != nil {
			return nil, err
		}
		if err = m.save(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if err = json.Unmarshal(data, &m.state); err != nil {
		return nil, err
	}
	cert, err := parse(m.state.Active)
	if err != nil {
		return nil, err
	}
	if err = cert.Leaf.VerifyHostname(o.Host); err != nil {
		return nil, errors.New("stored certificate does not match -cert-host; use a separate certificate directory")
	}
	if m.state.Next != "" {
		if _, err = parse(m.state.Next); err != nil {
			return nil, err
		}
	}
	m.current.Store(cert)
	return m, nil
}
func parse(bundle string) (*tls.Certificate, error) {
	c, err := tls.X509KeyPair([]byte(bundle), []byte(bundle))
	if err != nil {
		return nil, err
	}
	c.Leaf, err = x509.ParseCertificate(c.Certificate[0])
	return &c, err
}
func generate(host string, now time.Time) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", err
	}
	start := now.Add(-time.Minute)
	c := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, NotBefore: start, NotAfter: start.Add(lifetime), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		c.IPAddresses = []net.IP{ip}
	} else {
		c.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) + string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})), nil
}
func (m *Manager) save() error {
	data, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.opts.Directory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(m.opts.Directory, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(m.opts.Directory)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return m.current.Load().(*tls.Certificate), nil
}
func pin(c *tls.Certificate) Pin {
	sum := sha256.Sum256(c.Certificate[0])
	return Pin{Algorithm: "sha-256", Value: base64.StdEncoding.EncodeToString(sum[:]), NotBefore: c.Leaf.NotBefore, NotAfter: c.Leaf.NotAfter}
}
func (m *Manager) publish(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, m.opts.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid certificate webhook request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.opts.Client.Do(req)
	if err != nil {
		return errors.New("certificate webhook request failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.StatusCode == 202 {
		return fmt.Errorf("certificate webhook did not confirm storage (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// Step is called serially by Run. Successful publication always precedes activation.
func (m *Manager) Step(ctx context.Context, now time.Time) error {
	active := m.current.Load().(*tls.Certificate)
	if m.state.Next != "" {
		next, _ := parse(m.state.Next)
		if next.Leaf.NotAfter.Sub(now) <= renewBefore {
			m.state.Next = ""
			m.announcedAt = time.Time{}
			m.published = ""
		}
	}
	if active.Leaf.NotAfter.Sub(now) <= renewBefore && m.state.Next == "" {
		next, err := generate(m.opts.Host, now)
		if err != nil {
			return err
		}
		m.state.Next = next
		if err = m.save(); err != nil {
			m.state.Next = ""
			return err
		}
	}
	payload := Payload{Version: 1, Certificates: []Pin{pin(active)}}
	if m.state.Next != "" {
		next, _ := parse(m.state.Next)
		payload.Certificates = append(payload.Certificates, pin(next))
	}
	body, _ := json.Marshal(payload)
	// Retry unchanged payloads until confirmed; reset publication state on restart.
	if string(body) != m.published {
		if err := m.publish(ctx, body); err != nil {
			return err
		}
		m.published = string(body)
		if m.state.Next != "" {
			m.announcedAt = now
		}
		log.Print("certificate pins published")
	}
	if m.state.Next != "" && !m.announcedAt.IsZero() && now.Sub(m.announcedAt) >= m.opts.PublishDelay {
		next, _ := parse(m.state.Next)
		old := m.state
		m.state = state{Active: m.state.Next}
		if err := m.save(); err != nil {
			m.state = old
			return err
		}
		m.current.Store(next)
		m.published = ""
		m.announcedAt = time.Time{}
		log.Print("TLS certificate rotated")
	}
	return nil
}
func (m *Manager) Run(ctx context.Context) {
	for {
		if err := m.Step(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("certificate maintenance: %v; retrying in one minute", err)
		}
		timer := time.NewTimer(time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
