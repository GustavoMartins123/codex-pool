package runner

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"net/netip"
	"time"
)

// bootstrapIdentity is a throwaway certificate for one QR. Its SPKI hash
// goes into the QR, so the app trusts this certificate and nothing else.
type bootstrapIdentity struct {
	certificate tls.Certificate
	pin         string
}

func newBootstrapIdentity(addrs []netip.Addr, now, notAfter time.Time) (bootstrapIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return bootstrapIdentity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return bootstrapIdentity{}, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ips = append(ips, net.IP(addr.AsSlice()))
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "codex-pool runner bootstrap"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return bootstrapIdentity{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return bootstrapIdentity{}, err
	}
	return bootstrapIdentity{
		certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		pin:         spkiPin(leaf),
	}, nil
}

func spkiPin(certificate *x509.Certificate) string {
	digest := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (i bootstrapIdentity) serverConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{i.certificate},
		NextProtos:   []string{"http/1.1"},
		// TLS 1.2 stays for older phones, limited to forward-secret AEAD suites.
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}
}
