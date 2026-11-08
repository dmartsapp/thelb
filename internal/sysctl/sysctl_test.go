package sysctl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureAndRestore(t *testing.T) {
	Root = t.TempDir()
	put := func(name, v string) {
		p := filepath.Join(Root, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(v+"\n"), 0o644)
	}
	put("net/ipv4/conf/all/arp_ignore", "0")
	put("net/ipv4/conf/all/arp_announce", "2")
	var c Changes
	if old, changed, err := c.Ensure("net/ipv4/conf/all/arp_ignore", "1", "1", "2"); err != nil || !changed || old != "0" {
		t.Errorf("arp_ignore: %q %v %v", old, changed, err)
	}
	if _, changed, err := c.Ensure("net/ipv4/conf/all/arp_announce", "2"); err != nil || changed {
		t.Errorf("arp_announce, already right, was changed: %v %v", changed, err)
	}
	if v, _ := Read("net/ipv4/conf/all/arp_ignore"); v != "1" {
		t.Errorf("arp_ignore is %q", v)
	}
	r := c.Restore()
	if len(r) != 1 || r[0].Err != nil {
		t.Errorf("restored %+v", r)
	}
	if v, _ := Read("net/ipv4/conf/all/arp_ignore"); v != "0" {
		t.Errorf("arp_ignore after restore: %q", v)
	}
	if v, _ := Read("net/ipv4/conf/all/arp_announce"); v != "2" {
		t.Errorf("the user's arp_announce was touched: %q", v)
	}
}
