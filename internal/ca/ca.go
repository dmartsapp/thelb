// Package ca is the cluster's certificate authority. It lives in memory only:
// its key is never written, printed or logged.
package ca

import (
	"crypto"
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
	"strings"
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

// Issue creates a key for a node and signs its certificate: role is "director"
// or "backend", ips are the addresses it may present.
func (a *Authority) Issue(role, name string, ips []netip.Addr, now time.Time, lifetime time.Duration) (*Certificate, error) {
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	cert, der, err := a.Sign(role, name, ips, &key.PublicKey, now, lifetime)
	if err != nil {
		return nil, err
	}
	return &Certificate{Key: key, Cert: cert, DER: der}, nil
}

// Sign signs a certificate for a node's public key - the key stays with the
// node, which sent only a certificate signing request.
func (a *Authority) Sign(role, name string, ips []netip.Addr, pub crypto.PublicKey, now time.Time, lifetime time.Duration) (*x509.Certificate, []byte, error) {
	if role != "director" && role != "backend" {
		return nil, nil, errors.New("a certificate is for a director or a backend")
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, pub, a.key)
	if err != nil {
		return nil, nil, fmt.Errorf("signing a certificate for %s: %w", name, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("reading back the certificate for %s: %w", name, err)
	}
	return cert, der, nil
}

// NewKey is a node's key: ECDSA P-256, in memory.
func NewKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("creating a key: %w", err)
	}
	return key, nil
}

// Load is the cluster's CA from its certificate and key, as Raft replicates
// them to every director.
func Load(certDER, keyDER []byte) (*Authority, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("the CA's certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return nil, errors.New("the CA's key cannot be read")
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("the CA's key does not match its certificate")
	}
	cluster, ok := ClusterOf(cert)
	if !ok || !cert.IsCA {
		return nil, errors.New("the certificate is not a theLB CA's")
	}
	return &Authority{cluster: cluster, key: key, Cert: cert, DER: certDER}, nil
}

// KeyDER is the CA's key, for Raft to replicate to the directors - and nowhere
// else.
func (a *Authority) KeyDER() ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(a.key)
	if err != nil {
		return nil, fmt.Errorf("encoding the CA's key: %w", err)
	}
	return der, nil
}

// Cluster is the cluster's name, from the CA.
func (a *Authority) Cluster() string { return a.cluster }

// Pool is a certificate pool holding the CA alone.
func (a *Authority) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(a.Cert)
	return p
}

// Request is a certificate signing request for a node's key.
func Request(key *ecdsa.PrivateKey, name string) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, fmt.Errorf("creating a certificate signing request: %w", err)
	}
	return der, nil
}

// ParseRequest reads a certificate signing request and checks its signature: the
// sender holds the key.
func ParseRequest(der []byte) (*x509.CertificateRequest, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("the certificate signing request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the certificate signing request's signature: %w", err)
	}
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, errors.New("the certificate signing request is not for an ECDSA key")
	}
	return csr, nil
}

// Identity is a node's name in a certificate:
// spiffe://dmarts.app/thelb/<cluster>/<role>/<name>.
func Identity(cluster, role, name string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: "dmarts.app", Path: "/thelb/" + cluster + "/" + role + "/" + name}
}

// Peer is who a certificate names: its cluster, role and name, from its SPIFFE
// ID.
type Peer struct {
	Cluster, Role, Name string
}

// PeerOf reads a node certificate's identity.
func PeerOf(cert *x509.Certificate) (Peer, bool) {
	if len(cert.URIs) != 1 {
		return Peer{}, false
	}
	u := cert.URIs[0]
	if u.Scheme != "spiffe" || u.Host != "dmarts.app" {
		return Peer{}, false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "thelb" || (parts[2] != "director" && parts[2] != "backend") || parts[3] == "" {
		return Peer{}, false
	}
	return Peer{Cluster: parts[1], Role: parts[2], Name: parts[3]}, true
}

// ClusterOf reads the cluster's name from its CA's certificate.
func ClusterOf(cert *x509.Certificate) (string, bool) {
	if len(cert.URIs) != 1 {
		return "", false
	}
	u := cert.URIs[0]
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if u.Scheme != "spiffe" || u.Host != "dmarts.app" || len(parts) != 2 || parts[0] != "thelb" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// Serial is a certificate's serial number, in hexadecimal.
func Serial(cert *x509.Certificate) string { return cert.SerialNumber.Text(16) }

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
