package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TLS material lives next to the users file. The CA is generated once and
// is what invites carry; the server certificate is reissued whenever the
// set of names it must cover changes, so a laptop that moved to another
// network gets a certificate for its new address at the next start.
const (
	caCertFile     = "ca.crt"
	caKeyFile      = "ca.key"
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"
	caValidity     = 10 * 365 * 24 * time.Hour
	serverValidity = 2 * 365 * 24 * time.Hour
)

// ca is the loaded or freshly generated certificate authority.
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// der is the certificate as invites carry it.
	der []byte
}

// loadOrCreateCA reads the CA from dir or generates it.
func loadOrCreateCA(dir string, now time.Time) (*ca, error) {
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, err := parseCert(certPEM)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", certPath, err)
		}
		key, err := parseKey(keyPEM)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyPath, err)
		}
		return &ca{cert: cert, key: key, der: cert.Raw}, nil
	}
	if !errors.Is(certErr, os.ErrNotExist) && certErr != nil {
		return nil, certErr
	}
	if !errors.Is(keyErr, os.ErrNotExist) && keyErr != nil {
		return nil, keyErr
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "wormhole-ca", Organization: []string{"wormhole"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER); err != nil {
		return nil, err
	}
	return &ca{cert: cert, key: key, der: der}, nil
}

// Fingerprint is the SHA-256 of the CA certificate, for logs and doctor.
func (c *ca) Fingerprint() string {
	sum := sha256.Sum256(c.der)
	return hex.EncodeToString(sum[:])
}

// serverCert returns a certificate for the given names, reusing the one
// on disk when it was issued by this CA, is still valid for six months and
// covers every name.
func (c *ca) serverCert(dir string, dnsNames []string, ips []net.IP, now time.Time) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, serverCertFile), filepath.Join(dir, serverKeyFile)
	if existing, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(existing.Certificate[0]); err == nil && c.covers(leaf, dnsNames, ips, now) {
			return existing, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "wormhole-bus", Organization: []string{"wormhole"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER); err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// covers reports whether leaf was issued by this CA, outlives now by six
// months and names every dnsName and ip.
func (c *ca) covers(leaf *x509.Certificate, dnsNames []string, ips []net.IP, now time.Time) bool {
	if err := leaf.CheckSignatureFrom(c.cert); err != nil {
		return false
	}
	if now.Add(180 * 24 * time.Hour).After(leaf.NotAfter) {
		return false
	}
	have := map[string]bool{}
	for _, n := range leaf.DNSNames {
		have[strings.ToLower(n)] = true
	}
	for _, ip := range leaf.IPAddresses {
		have[ip.String()] = true
	}
	for _, n := range dnsNames {
		if !have[strings.ToLower(n)] {
			return false
		}
	}
	for _, ip := range ips {
		if !have[ip.String()] {
			return false
		}
	}
	return true
}

// writePEM writes one PEM block with private modes.
func writePEM(path, typ string, der []byte) error {
	return writePrivate(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

// parseCert decodes a PEM certificate.
func parseCert(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// parseKey decodes a PEM EC private key.
func parseKey(p []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, errors.New("not a PEM EC private key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// CAPEM encodes a DER certificate as PEM, the form config files point at.
func CAPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// localNames lists the names a server certificate must cover: localhost,
// the hostname and every non-loopback unicast address of this machine,
// plus the hosts in extra (names or addresses, with or without a port).
func localNames(hostname string, extra []string) (dns []string, ips []net.IP) {
	seenDNS := map[string]bool{}
	seenIP := map[string]bool{}
	addDNS := func(n string) {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seenDNS[n] {
			return
		}
		seenDNS[n] = true
		dns = append(dns, n)
	}
	addIP := func(ip net.IP) {
		if ip == nil || seenIP[ip.String()] {
			return
		}
		seenIP[ip.String()] = true
		ips = append(ips, ip)
	}
	addDNS("localhost")
	addIP(net.IPv4(127, 0, 0, 1))
	addIP(net.IPv6loopback)
	addDNS(hostname)
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || !ipn.IP.IsGlobalUnicast() && !ipn.IP.IsPrivate() {
				continue
			}
			addIP(ipn.IP)
		}
	}
	for _, e := range extra {
		host := e
		if h, _, err := net.SplitHostPort(e); err == nil {
			host = h
		}
		if ip := net.ParseIP(host); ip != nil {
			addIP(ip)
		} else {
			addDNS(host)
		}
	}
	// Loopback first, then the rest in a stable order.
	if len(ips) > 2 {
		rest := ips[2:]
		sort.Slice(rest, func(i, j int) bool { return rest[i].String() < rest[j].String() })
	}
	return dns, ips
}
