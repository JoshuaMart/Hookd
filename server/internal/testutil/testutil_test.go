package testutil

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"strconv"
	"testing"
)

func TestWriteKeyPair(t *testing.T) {
	certFile, keyFile, roots := WriteKeyPair(t, "api.test", "127.0.0.1")
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api.test", "127.0.0.1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestFreePort(t *testing.T) {
	port := FreePort(t)
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("port %d not free: %v", port, err)
	}
	l.Close()
}
