package certmanager

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRotationPersistsAndPublishesBeforeActivation(t *testing.T) {
	var payloads []Payload
	fail := false
	o := Options{Host: "nas.example", Directory: t.TempDir(), WebhookURL: "https://hooks.example/private-secret", PublishDelay: time.Hour, Client: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if fail {
			return nil, errors.New("failed https://hooks.example/private-secret")
		}
		if r.Method != "PUT" {
			t.Fatal(r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "PRIVATE KEY") {
			t.Fatal("private key sent")
		}
		var p Payload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, p)
		return &http.Response{StatusCode: 204, Body: http.NoBody, Header: make(http.Header)}, nil
	})}}
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := m.GetCertificate(&tls.ClientHelloInfo{})
	firstPin := pin(current).Value
	now := time.Now()
	ctx := context.Background()
	if err = m.Step(ctx, now); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(o.Directory, "state.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	fail = true
	renewal := current.Leaf.NotAfter.Add(-24 * time.Hour)
	err = m.Step(ctx, renewal)
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal(err)
	}
	if m.state.Next == "" {
		t.Fatal("pending certificate not persisted")
	}
	got, _ := m.GetCertificate(nil)
	if pin(got).Value != firstPin {
		t.Fatal("activated before webhook acknowledgement")
	}
	// Restart must retain both keys, republish, and start a fresh propagation delay.
	m, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	fail = false
	if err = m.Step(ctx, renewal); err != nil {
		t.Fatal(err)
	}
	if len(payloads[len(payloads)-1].Certificates) != 2 {
		t.Fatal("missing overlap pins")
	}
	if err = m.Step(ctx, renewal.Add(59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetCertificate(nil)
	if pin(got).Value != firstPin {
		t.Fatal("activated before delay")
	}
	if err = m.Step(ctx, renewal.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetCertificate(nil)
	if pin(got).Value == firstPin {
		t.Fatal("rotation did not activate")
	}
	reloaded, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := reloaded.GetCertificate(nil)
	if pin(saved).Value != pin(got).Value {
		t.Fatal("rotation not persisted")
	}
}
func TestRejectInsecureWebhook(t *testing.T) {
	for _, u := range []string{"http://example.com/secret", "https://user:pass@example.com/hook", "https://example.com/#secret"} {
		_, err := New(Options{Host: "nas.example", Directory: t.TempDir(), WebhookURL: u, PublishDelay: time.Hour})
		if err == nil {
			t.Fatal(u)
		}
	}
}
func TestRejectUnconfirmedWebhook(t *testing.T) {
	for _, status := range []int{202, 301, 400, 500} {
		m, err := New(Options{Host: "nas.example", Directory: t.TempDir(), WebhookURL: "https://example.com/secret", PublishDelay: time.Hour, Client: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: http.NoBody}, nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		if err = m.Step(context.Background(), time.Now()); err == nil {
			t.Fatal(status)
		}
	}
}

func TestWebhookNeverFollowsRedirects(t *testing.T) {
	calls := 0
	m, err := New(Options{Host: "nas.example", Directory: t.TempDir(), WebhookURL: "https://example.com/private-secret", PublishDelay: time.Hour, Client: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://other.example/leak"}}, Body: http.NoBody}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Step(context.Background(), time.Now()); err == nil {
		t.Fatal("redirect accepted")
	}
	if calls != 1 {
		t.Fatal("redirect followed")
	}
}

func TestHostIsOptionalAndExistingCertificatesAreReused(t *testing.T) {
	options := Options{Directory: t.TempDir(), WebhookURL: "https://example.com/private-secret", PublishDelay: time.Hour}
	m, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := m.GetCertificate(nil)
	if err := cert.Leaf.VerifyHostname(DefaultHost); err != nil {
		t.Fatal(err)
	}
	savedPin := pin(cert).Value
	reloaded, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ = reloaded.GetCertificate(nil)
	if pin(cert).Value != savedPin {
		t.Fatal("default-host restart changed certificate")
	}

	options.Directory = t.TempDir()
	options.Host = "previously-configured.example"
	m, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ = m.GetCertificate(nil)
	savedPin = pin(cert).Value
	options.Host = ""
	reloaded, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ = reloaded.GetCertificate(nil)
	if pin(cert).Value != savedPin {
		t.Fatal("removing host configuration changed certificate")
	}
	options.Host = "different.example"
	if _, err = New(options); err == nil {
		t.Fatal("explicit hostname mismatch accepted")
	}
}
