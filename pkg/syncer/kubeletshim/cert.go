package kubeletshim

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// loadCA reads a PEM-encoded CA certificate and private key from disk. The
// private key file may be in PKCS#8, PKCS#1 (RSA) or SEC1 (EC) form — k3s
// uses ECDSA, but we accept the others to keep this independent of k3s
// internals.
func loadCA(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading CA cert %s: %w", certPath, err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("decoding CA cert %s: no PEM block", certPath)
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA cert %s: %w", certPath, err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading CA key %s: %w", keyPath, err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("decoding CA key %s: no PEM block", keyPath)
	}

	if key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err == nil {
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, nil, errors.New("CA key is not a crypto.Signer")
		}
		return caCert, signer, nil
	}
	if key, err := x509.ParseECPrivateKey(keyBlock.Bytes); err == nil {
		return caCert, key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err == nil {
		return caCert, key, nil
	}
	return nil, nil, fmt.Errorf("CA key %s: unsupported format", keyPath)
}

// generateServingCert produces a leaf TLS certificate signed by caCert/caKey
// suitable for serving HTTPS on the given pod IP. The kube-apiserver verifies
// the dialed kubelet's TLS against its --kubelet-certificate-authority, which
// k3s sets to its server-ca, so the cert must be signed by that CA.
func generateServingCert(podIP string, caCert *x509.Certificate, caKey crypto.Signer) (tls.Certificate, error) {
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating leaf key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating serial: %w", err)
	}

	ips := []net.IP{net.ParseIP("127.0.0.1")}
	if podIP != "" {
		if ip := net.ParseIP(podIP); ip != nil {
			ips = append(ips, ip)
		}
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "vibecluster-kubelet-shim"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("signing leaf cert: %w", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  leafKey,
		Leaf:        nil, // not needed; tls.Server reparses on demand
	}, nil
}

