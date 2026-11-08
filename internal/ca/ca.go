// Package ca is the cluster's certificate authority. It lives in memory only:
// its key is never written, printed or logged.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"time"
)

// Lifetimes: the CA lives as long as the cluster (in memory, so until every
// director stops), a node's certificate for a day.
const (
	CALifetime   = 10 * 365 * 24 * time.Hour
	NodeLifetime = 24 * time.Hour
)

// Authority is a cluster's CA.
type Authority struct {
	cluster string
	key     *ecdsa.PrivateKey
	Cert    *x509.Certificate
	DER     []byte
}

// Certificate is a node's certificate, with its key.
type Certificate struct {
	Key  *ecdsa.PrivateKey
	Cert *x509.Certificate
	DER  []byte
}

// New creates the CA of cluster.
func New(cluster string, now time.Time) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("creating the CA's key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"theLB"}, CommonName: "theLB " + cluster + " CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(CALifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		URIs:                  []*url.URL{{Scheme: "spiffe", Host: "dmarts.app", Path: "/thelb/" + cluster}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating the CA's certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("reading back the CA's certificate: %w", err)
	}
	return &Authority{cluster: cluster, key: key, Cert: cert, DER: der}, nil
}

// Issue signs a certificate for a node: role is "director" or "backend", ips are
// the addresses it may present.
func (a *Authority) Issue(role, name string, ips []netip.Addr, now time.Time, lifetime time.Duration) (*Certificate, error) {
	if role != "director" && role != "backend" {
		return nil, errors.New("a certificate is for a director or a backend")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("creating a node key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"theLB"}, OrganizationalUnit: []string{role}, CommonName: name},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{Identity(a.cluster, role, name)},
	}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.IP(ip.Unmap().AsSlice()))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, fmt.Errorf("signing a certificate for %s: %w", name, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("reading back the certificate for %s: %w", name, err)
	}
	return &Certificate{Key: key, Cert: cert, DER: der}, nil
}

// Identity is a node's name in a certificate:
// spiffe://dmarts.app/thelb/<cluster>/<role>/<name>.
func Identity(cluster, role, name string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: "dmarts.app", Path: "/thelb/" + cluster + "/" + role + "/" + name}
}

// Fingerprint is a certificate's SHA-256, in hexadecimal.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("creating a serial number: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}
