//go:build ignore

// Generates a self-signed TLS cert for the podgate-webhook Service, valid for
// one year. Run with: go run hack/gencert.go <dns-name> <out-dir>
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func main() {
	dnsName := os.Args[1]
	outDir := os.Args[2]
	dnsNames := []string{dnsName, dnsName + ".cluster.local"}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	must(err)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(err)

	must(os.MkdirAll(outDir, 0o755))

	certOut, err := os.Create(filepath.Join(outDir, "tls.crt"))
	must(err)
	must(pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}))
	must(certOut.Close())

	keyBytes, err := x509.MarshalECPrivateKey(key)
	must(err)
	keyOut, err := os.Create(filepath.Join(outDir, "tls.key"))
	must(err)
	must(pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}))
	must(keyOut.Close())

	fmt.Println("wrote", filepath.Join(outDir, "tls.crt"), "and tls.key for", dnsName)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
