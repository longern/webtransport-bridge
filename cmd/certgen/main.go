// certgen creates a short-lived ECDSA certificate and prints its browser pin.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func main() {
	host := flag.String("host", "localhost", "public hostname or IP")
	out := flag.String("out", "certs", "output directory (must not already contain certificate files)")
	flag.Parse()
	if err := os.MkdirAll(*out, 0700); err != nil {
		log.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: *host}, NotBefore: now, NotAfter: now.Add(13 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	if ip := net.ParseIP(*host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{*host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		log.Fatal(err)
	}
	for _, item := range []struct {
		name, kind string
		data       []byte
	}{{"key.pem", "PRIVATE KEY", priv}, {"cert.pem", "CERTIFICATE", der}} {
		f, err := os.OpenFile(filepath.Join(*out, item.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			log.Fatal(err)
		}
		if err := pem.Encode(f, &pem.Block{Type: item.kind, Bytes: item.data}); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}
	sum := sha256.Sum256(der)
	fmt.Printf("sha256 base64: %s\nexpires: %s\n", base64.StdEncoding.EncodeToString(sum[:]), template.NotAfter.Format(time.RFC3339))
}
