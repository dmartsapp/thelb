// Package fingerprint computes a node's fingerprint: what a director approves,
// and recognises when an approved node comes back. It is a SHA-256 hash, shown
// as its first 16 hexadecimal digits, of
//
//   - the machine's application-specific ID: an HMAC-SHA256 keyed with
//     /etc/machine-id over theLB's own application ID, as systemd recommends, so
//     the machine ID itself never leaves the machine;
//   - the SMBIOS system UUID, where the machine has one;
//   - the MAC address of the interface the node joins through.
//
// The hostname is shown beside it, as a label, and is not part of it.
package fingerprint

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// appID is theLB's application ID, the message the machine ID keys. It is fixed
// for good: changing it would change every node's fingerprint.
var appID = [16]byte{0x7d, 0x2a, 0x91, 0x4c, 0xe3, 0x08, 0x4f, 0x6b, 0xb5, 0x1d, 0x63, 0xc0, 0x9e, 0x47, 0x2f, 0xa8}

// Inputs are what a fingerprint is made of, and the hostname shown beside it.
type Inputs struct {
	MachineID  [16]byte
	SMBIOSUUID string
	MAC        string
	Hostname   string
}

// AppSpecificID is the machine's ID as theLB uses it - never the ID itself.
func AppSpecificID(machineID [16]byte) [32]byte {
	m := hmac.New(sha256.New, machineID[:])
	m.Write(appID[:])
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// Compute returns the fingerprint of in.
func Compute(in Inputs) string {
	app := AppSpecificID(in.MachineID)
	h := sha256.New()
	h.Write([]byte("thelb-node-fingerprint-v1"))
	h.Write([]byte{0})
	h.Write(app[:])
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(in.SMBIOSUUID))))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(in.MAC))))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ParseMachineID reads the contents of /etc/machine-id: 32 hexadecimal digits.
func ParseMachineID(text string) ([16]byte, error) {
	var id [16]byte
	t := strings.TrimSpace(text)
	if len(t) != 32 {
		return id, errors.New("the machine ID is not 32 hexadecimal digits")
	}
	if _, err := hex.Decode(id[:], []byte(t)); err != nil {
		return id, errors.New("the machine ID is not 32 hexadecimal digits")
	}
	if id == ([16]byte{}) {
		return id, errors.New("the machine ID is all zeros: it was never set (systemd-machine-id-setup sets it)")
	}
	return id, nil
}
