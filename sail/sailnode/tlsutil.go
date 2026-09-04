package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
)

type tlsCert = tls.Certificate

func tlsX509KeyPair(certPEM, keyPEM []byte) (tls.Certificate, error) {
	return tls.X509KeyPair(certPEM, keyPEM)
}

// pemOf re-encodes a certificate for persistence.
func pemOf(c tls.Certificate) (certPEM, keyPEM []byte) {
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})
	if k, ok := c.PrivateKey.(interface{ Bytes() []byte }); ok {
		_ = k
	}
	der, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		return certPEM, nil
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return
}

// tlsLoadX509KeyPair loads a PEM certificate chain and key from files.
func tlsLoadX509KeyPair(certFile, keyFile string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(certFile, keyFile)
}
