package ca

import (
	"crypto/ecdsa"
	"testing"
)

// FuzzParseRequest: a joiner sends its certificate signing request before it
// is approved. Whatever ParseRequest takes is signed by its own key, and that
// key is ECDSA - the only kind the cluster signs.
func FuzzParseRequest(f *testing.F) {
	key, err := NewKey()
	if err != nil {
		f.Fatal(err)
	}
	der, err := Request(key, "web-1")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(der)
	f.Add(der[:len(der)/2])
	f.Add([]byte{0x30, 0x82, 0xff, 0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		csr, err := ParseRequest(b)
		if err != nil {
			return
		}
		if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
			t.Fatalf("took a request for a %T key", csr.PublicKey)
		}
		if csr.CheckSignature() != nil {
			t.Fatal("took a request whose signature does not check")
		}
	})
}
