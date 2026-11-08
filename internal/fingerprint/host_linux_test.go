//go:build linux

package fingerprint

import (
	"os"
	"testing"
)

func TestReadHostOnThisMachine(t *testing.T) {
	if _, err := os.Stat("/etc/machine-id"); err != nil {
		t.Skip("this machine has no /etc/machine-id")
	}
	in, err := ReadHost("52:54:00:3a:19:04")
	if err != nil {
		t.Fatal(err)
	}
	if in.MachineID == ([16]byte{}) || in.MAC == "" {
		t.Errorf("incomplete inputs: %+v", in)
	}
	again, _ := ReadHost("52:54:00:3a:19:04")
	if Compute(in) != Compute(again) {
		t.Error("this machine's fingerprint changes between two reads")
	}
}
