package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	caCertFile     = "ca.crt"
	caKeyFile      = "ca.key"
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"

	caValidity     = 10 * 365 * 24 * time.Hour
	serverValidity = 825 * 24 * time.Hour
	// serverRenewBefore regenerates a server certificate this close to expiry.
	serverRenewBefore = 30 * 24 * time.Hour
)

// authority is the bus CA.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// loadOrCreateCA reads the CA from dir, generating it on first use.
func loadOrCreateCA(dir string, now time.Time) (*authority, error) {
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	cert, cerr := readCert(certPath)
	key, kerr := readKey(keyPath)
	if cerr == nil && kerr == nil {
		return &authority{cert: cert, key: key}, nil
	}
	if !errors.Is(cerr, os.ErrNotExist) || !errors.Is(kerr, os.ErrNotExist) {
		if cerr != nil {
			return nil, fmt.Errorf("CA: %w (move %s aside to generate a new CA; every joined machine then needs a new invite)", cerr, dir)
		}
		return nil, fmt.Errorf("CA: %w (move %s aside to generate a new CA; every joined machine then needs a new invite)", kerr, dir)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "wormhole-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	if err := writeKey(keyPath, key); err != nil {
		return nil, err
	}
	if err := writeFile(certPath, pemCert(der)); err != nil {
		return nil, err
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &authority{cert: cert, key: key}, nil
}

// ensureServerCert returns a TLS certificate for names, reusing the one on
// disk when it was issued by ca for exactly the same names and is not
// about to expire.
func ensureServerCert(dir string, ca *authority, dnsNames []string, ips []net.IP, now time.Time) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, serverCertFile), filepath.Join(dir, serverKeyFile)
	if cert, err := readCert(certPath); err == nil && sameNames(cert, dnsNames, ips) &&
		cert.CheckSignatureFrom(ca.cert) == nil && now.Add(serverRenewBefore).Before(cert.NotAfter) {
		if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			return pair, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "wormhole-serve"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writeKey(keyPath, key); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFile(certPath, pemCert(der)); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// sameNames reports whether cert carries exactly dnsNames and ips.
func sameNames(cert *x509.Certificate, dnsNames []string, ips []net.IP) bool {
	have := slices.Clone(cert.DNSNames)
	want := slices.Clone(dnsNames)
	slices.Sort(have)
	slices.Sort(want)
	if !slices.Equal(have, want) {
		return false
	}
	hs, ws := ipStrings(cert.IPAddresses), ipStrings(ips)
	return slices.Equal(hs, ws)
}

// ipStrings renders ips sorted, for comparison.
func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	slices.Sort(out)
	return out
}

// serial returns a random 128-bit certificate serial number.
func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}

// pemCert encodes a DER certificate as PEM.
func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// readCert parses the first PEM certificate in path.
func readCert(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no PEM certificate", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

// readKey parses an EC private key from path.
func readKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM key", path)
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// writeKey stores key as a private PEM file.
func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writeFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// CACertDER returns the DER bytes of the CA in a serve state directory.
func CACertDER(stateDir string) ([]byte, error) {
	cert, err := readCert(filepath.Join(stateDir, caCertFile))
	if err != nil {
		return nil, err
	}
	return cert.Raw, nil
}

// CAPEM converts the DER certificate an invite carries to PEM.
func CAPEM(der []byte) []byte { return pemCert(der) }
