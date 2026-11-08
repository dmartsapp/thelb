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
