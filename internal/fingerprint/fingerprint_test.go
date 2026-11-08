package fingerprint

import (
	"regexp"
	"testing"
)

func sample() Inputs {
	id, _ := ParseMachineID("4c2f3a0e9b7d4e1fa6c58d2b91e0f7a3")
	return Inputs{MachineID: id, SMBIOSUUID: "6F2E1C44-90AB-4C3D-8E7F-1234567890AB", MAC: "52:54:00:3A:19:04", Hostname: "lb-1"}
}

func TestComputeIsStableAndShort(t *testing.T) {
	a, b := Compute(sample()), Compute(sample())
	if a != b {
		t.Fatalf("not stable: %s, %s", a, b)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a) {
		t.Errorf("%q is not 16 hexadecimal digits", a)
	}
}

func TestEveryInputButTheHostnameChangesIt(t *testing.T) {
	base := Compute(sample())
	in := sample()
	in.MachineID[0] ^= 1
	if Compute(in) == base {
		t.Error("the machine ID does not change the fingerprint")
	}
	in = sample()
	in.SMBIOSUUID = ""
	if Compute(in) == base {
		t.Error("the SMBIOS UUID does not change the fingerprint")
	}
	in = sample()
	in.MAC = "52:54:00:3a:19:05"
	if Compute(in) == base {
		t.Error("the MAC address does not change the fingerprint")
	}
	in = sample()
	in.Hostname = "renamed"
	if Compute(in) != base {
		t.Error("the hostname changes the fingerprint")
	}
	in = sample()
	in.MAC, in.SMBIOSUUID = "52:54:00:3a:19:04", "6f2e1c44-90ab-4c3d-8e7f-1234567890ab"
	if Compute(in) != base {
		t.Error("letter case changes the fingerprint")
	}
}

func TestTheMachineIDIsUsedOnlyThroughTheApplicationID(t *testing.T) {
	in := sample()
	app := AppSpecificID(in.MachineID)
	if string(app[:16]) == string(in.MachineID[:]) {
		t.Error("the application-specific ID is the machine ID")
	}
	other := in.MachineID
	other[15] ^= 0xff
	if AppSpecificID(other) == app {
		t.Error("two machine IDs give the same application-specific ID")
	}
}

func TestParseMachineID(t *testing.T) {
	for _, bad := range []string{"", "xyz", "4c2f3a0e9b7d4e1fa6c58d2b91e0f7a", "zz2f3a0e9b7d4e1fa6c58d2b91e0f7a3", "00000000000000000000000000000000"} {
		if _, err := ParseMachineID(bad); err == nil {
			t.Errorf("ParseMachineID(%q) accepted it", bad)
		}
	}
	if _, err := ParseMachineID("4c2f3a0e9b7d4e1fa6c58d2b91e0f7a3\n"); err != nil {
		t.Errorf("a machine-id file's contents: %v", err)
	}
}
