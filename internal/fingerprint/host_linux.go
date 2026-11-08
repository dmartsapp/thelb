//go:build linux

package fingerprint

import (
	"fmt"
	"os"
	"strings"
)

// ReadHost reads this machine's fingerprint inputs; mac is the MAC address of
// the interface the node joins through.
func ReadHost(mac string) (Inputs, error) {
	b, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return Inputs{}, fmt.Errorf("reading the machine ID: %w", err)
	}
	id, err := ParseMachineID(string(b))
	if err != nil {
		return Inputs{}, fmt.Errorf("/etc/machine-id: %w", err)
	}
	in := Inputs{MachineID: id, MAC: mac}
	// Not every machine has an SMBIOS UUID; without one, the fingerprint is
	// made of the other two.
	if u, err := os.ReadFile("/sys/class/dmi/id/product_uuid"); err == nil {
		in.SMBIOSUUID = strings.TrimSpace(string(u))
	}
	if h, err := os.Hostname(); err == nil {
		in.Hostname = h
	}
	return in, nil
}
