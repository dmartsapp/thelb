package director

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

// TestStandAloneOnlyWhenTheOtherIsGone: the last of two directors stands alone
// when the other's theLB is not running, or its machine does not answer - and
// never while a theLB answers on the control VIP, another machine holds it,
// this director is cut off itself, or the other answers on the network while its
// port drops connections (a partition: it may be leading).
func TestStandAloneOnlyWhenTheOtherIsGone(t *testing.T) {
	refused := fmt.Errorf("dial tcp 192.168.1.92:8080: %w", syscall.ECONNREFUSED)
	timeout := errors.New("dial tcp 192.168.1.92:8080: i/o timeout")
	gone := takeoverFacts{other: "linux", otherMAC: "02:11:32:24:ac:11", uplink: true, portErr: refused}
	for _, c := range []struct {
		name string
		edit func(*takeoverFacts)
		want bool
		why  string
	}{
		{"its theLB was killed: the port refuses, the VIP is still on its interface", func(f *takeoverFacts) { f.vipHolder = "02:11:32:24:AC:11" }, true, "refuses connections"},
		{"its machine is down: nothing answers", func(f *takeoverFacts) { f.portErr = timeout }, true, "does not answer on the network"},
		{"a firewall drops its port: a partition", func(f *takeoverFacts) { f.portErr, f.answers = timeout, "02:11:32:24:ac:11" }, false, "a partition, not a crash"},
		{"its port accepts", func(f *takeoverFacts) { f.portErr = nil }, false, "accepts connections"},
		{"a theLB answers on the VIP", func(f *takeoverFacts) { f.vipTheLB = true }, false, "a theLB answers on the control VIP"},
		{"another machine holds the VIP", func(f *takeoverFacts) { f.vipHolder = "52:54:00:aa:bb:cc" }, false, "52:54:00:aa:bb:cc holds the control VIP"},
		{"this director is cut off", func(f *takeoverFacts) { f.uplink, f.uplinkWhy = false, "its gateway 192.168.1.1 does not answer" }, false, "does not reach its network: its gateway"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := gone
			c.edit(&f)
			got, why := standAlone(f)
			if got != c.want || !strings.Contains(why, c.why) {
				t.Errorf("standAlone = %v, %q; want %v, %q", got, why, c.want, c.why)
			}
		})
	}
}

func TestTakeoverIsAnError(t *testing.T) {
	var err error = &takeover{why: "linux does not answer on the network"}
	var tk *takeover
	if !errors.As(fmt.Errorf("session: %w", err), &tk) || tk.why == "" || errors.Is(err, errRejoin) {
		t.Errorf("a takeover is not told apart: %v", err)
	}
}
