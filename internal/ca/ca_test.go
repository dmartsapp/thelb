package ca

import (
	"crypto/x509"
	"net/netip"
	"regexp"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func TestTheCA(t *testing.T) {
	a, err := New("prod", now)
	if err != nil {
		t.Fatal(err)
	}
	c := a.Cert
	if !c.IsCA || !c.BasicConstraintsValid || !c.MaxPathLenZero {
		t.Error("not a CA that may sign only end certificates")
	}
	if c.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("the CA may not sign certificates")
	}
	if c.Subject.CommonName != "theLB prod CA" {
		t.Errorf("subject %q", c.Subject.CommonName)
	}
	if got := c.NotAfter.Sub(now); got < CALifetime-time.Minute || got > CALifetime {
		t.Errorf("lifetime %v", got)
	}
	if err := c.CheckSignatureFrom(c); err != nil {
		t.Errorf("not self-signed: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(Fingerprint(a.DER)) {
		t.Errorf("fingerprint %q", Fingerprint(a.DER))
	}
}

func TestIssueADirectorsCertificate(t *testing.T) {
	a, _ := New("prod", now)
	ips := []netip.Addr{netip.MustParseAddr("10.0.0.11"), netip.MustParseAddr("10.0.0.10"), netip.MustParseAddr("fd00::11")}
	n, err := a.Issue("director", "lb-1", ips, now, NodeLifetime)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(a.Cert)
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := n.Cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now.Add(time.Hour), KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			t.Errorf("does not verify for %v: %v", usage, err)
		}
	}
	if _, err := n.Cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now.Add(25 * time.Hour)}); err == nil {
		t.Error("still valid after a day")
	}
	if len(n.Cert.URIs) != 1 || n.Cert.URIs[0].String() != "spiffe://dmarts.app/thelb/prod/director/lb-1" {
		t.Errorf("identity %v", n.Cert.URIs)
	}
	if len(n.Cert.IPAddresses) != 3 || n.Cert.IPAddresses[1].String() != "10.0.0.10" {
		t.Errorf("addresses %v", n.Cert.IPAddresses)
	}
	if n.Cert.IsCA {
		t.Error("a node's certificate is a CA")
	}
	other, _ := New("prod", now)
	pool := x509.NewCertPool()
	pool.AddCert(other.Cert)
	if _, err := n.Cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now}); err == nil {
		t.Error("verifies against another cluster's CA")
	}
}

func TestIssueRefusesOtherRoles(t *testing.T) {
	a, _ := New("prod", now)
	if _, err := a.Issue("admin", "x", nil, now, time.Hour); err == nil {
		t.Error("issued a certificate for an unknown role")
	}
}

func TestTheCAComesBackFromWhatRaftReplicates(t *testing.T) {
	a, _ := New("prod", now)
	key, err := a.KeyDER()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(a.DER, key)
	if err != nil {
		t.Fatal(err)
	}
	if b.Cluster() != "prod" {
		t.Errorf("cluster %q", b.Cluster())
	}
	c, err := b.Issue("director", "lb-2", []netip.Addr{netip.MustParseAddr("10.0.0.12")}, now, NodeLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cert.Verify(x509.VerifyOptions{Roots: a.Pool(), CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("a certificate the loaded CA signed does not verify against the original: %v", err)
	}

	other, _ := New("prod", now)
	otherKey, _ := other.KeyDER()
	if _, err := Load(a.DER, otherKey); err == nil {
		t.Error("a CA certificate loaded with another CA's key")
	}
	if _, err := Load(c.DER, key); err == nil {
		t.Error("a node's certificate loaded as a CA")
	}
}

func TestSignARequestKeepsTheKeyWithTheNode(t *testing.T) {
	a, _ := New("prod", now)
	key, _ := NewKey()
	der, err := Request(key, "lb-2")
	if err != nil {
		t.Fatal(err)
	}
	csr, err := ParseRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := a.Sign("director", "lb-2", []netip.Addr{netip.MustParseAddr("10.0.0.12")}, csr.PublicKey, now, NodeLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		t.Error("the certificate is not for the node's key")
	}
	p, ok := PeerOf(cert)
	if !ok || p != (Peer{Cluster: "prod", Role: "director", Name: "lb-2"}) {
		t.Errorf("peer %+v %v", p, ok)
	}
	if name, ok := ClusterOf(a.Cert); !ok || name != "prod" {
		t.Errorf("cluster of the CA: %q %v", name, ok)
	}
	if _, ok := PeerOf(a.Cert); ok {
		t.Error("the CA read as a node")
	}

	der[len(der)-1] ^= 0xff
	if _, err := ParseRequest(der); err == nil {
		t.Error("a request with a broken signature was accepted")
	}
}
