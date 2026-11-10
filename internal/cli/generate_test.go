package cli

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/hostnet"
)

// TestGenerateSystemd: the unit runs the role with the flags given, checked as
// the role checks them, under systemd's watchdog, its capabilities bounded and
// the system read-only but for what it writes - and sees the files it needs,
// wherever they are.
func TestGenerateSystemd(t *testing.T) {
	savedFor, savedExe := localFor, executable
	t.Cleanup(func() { localFor, executable = savedFor, savedExe })
	localFor = func(netip.Addr, string) (hostnet.Address, error) { return hostnet.Address{}, nil }
	executable = func() (string, error) { return "/usr/local/bin/thelb", nil }

	code, out, errOut := as(t, 0, "generate", "systemd", "director", "--vip", "10.0.0.10", "--port", "7443", "--bootstrap",
		"--secret-file", "/etc/thelb/director.secret", "--backend-secret-file", "/etc/thelb/backend.secret", "--name", "lb-1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"ExecStart=/usr/local/bin/thelb director --vip 10.0.0.10 --port 7443 --bootstrap --secret-file /etc/thelb/director.secret --backend-secret-file /etc/thelb/backend.secret --name lb-1\n",
		"Type=notify\n", "WatchdogSec=30\n", "Restart=always\n", "RestartSec=30\n",
		"CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE\n",
		"ProtectSystem=full\n", "ReadWritePaths=-/etc/thelb\n", "ProtectHome=yes\n", "PrivateTmp=yes\n",
		"[Install]\nWantedBy=multi-user.target\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the director's unit lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ProtectKernelTunables") || strings.Contains(out, "ProtectKernelModules") {
		t.Errorf("the unit would stop theLB writing /proc/sys or the kernel loading modules:\n%s", out)
	}

	code, out, errOut = as(t, 0, "generate", "systemd", "backend", "--join", "10.0.0.10:7443", "--secret-file", "/home/ops/backend.secret", "--offer", "web=tcp/80,check=http:/health")
	if code != 0 {
		t.Fatalf("backend: exit %d: %s", code, errOut)
	}
	for _, want := range []string{" backend --join 10.0.0.10:7443 --secret-file /home/ops/backend.secret --offer web=tcp/80,check=http:/health\n", "RestartSec=1\n", "ProtectHome=read-only\n", "PrivateTmp=yes\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the backend's unit lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ReadWritePaths") {
		t.Errorf("a backend writes nothing outside /run:\n%s", out)
	}
	_, out, _ = as(t, 0, "generate", "systemd", "backend", "--join", "10.0.0.10:7443", "--secret-file", "/tmp/backend")
	if strings.Contains(out, "PrivateTmp") || !strings.Contains(out, "does not survive a reboot") {
		t.Errorf("a secret file in /tmp: hidden by PrivateTmp, or no warning:\n%s", out)
	}

	if code, _, errOut := as(t, 0, "generate", "systemd", "director", "--port", "7443"); code != 2 || !strings.Contains(errOut, "thelb generate systemd director: --vip, the control VIP, is required") {
		t.Errorf("flags the director refuses: exit %d, %q", code, errOut)
	}
	if code, _, _ := as(t, 0, "generate", "systemd"); code != 2 {
		t.Errorf("no role: exit %d", code)
	}
}

func TestSystemdArgs(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/thelb": "/usr/local/bin/thelb",
		"web=tcp/80":           "web=tcp/80",
		"a b":                  `"a b"`,
		`say "hi"`:             `"say \"hi\""`,
		"50%":                  "50%%",
		"$HOME":                "$$HOME",
		";":                    `";"`,
	} {
		if got := systemdArg(in); got != want {
			t.Errorf("systemdArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerateSysctlAndModules(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"generate", "sysctl", "director"}, []string{"net.ipv4.vs.sloppy_tcp = 1", "/etc/sysctl.d/90-thelb-director.conf"}},
		{[]string{"generate", "sysctl", "backend"}, []string{"net.ipv4.conf.all.arp_ignore = 1", "net.ipv4.conf.all.arp_announce = 2"}},
		{[]string{"generate", "modules", "director"}, []string{"\nip_vs\n", "\nip_vs_wrr\n", "/etc/modules-load.d/thelb-director.conf"}},
		{[]string{"generate", "modules", "backend"}, []string{"\ndummy\n"}},
	} {
		code, out, errOut := as(t, 0, c.args...)
		for _, w := range c.want {
			if code != 0 || !strings.Contains(out, w) {
				t.Errorf("%v: exit %d %s, lacks %q:\n%s", c.args, code, errOut, w, out)
			}
		}
	}
	if code, _, _ := as(t, 0, "generate", "sysctl", "both"); code != 2 {
		t.Errorf("an unknown role: exit %d", code)
	}
}

func TestDoctorFlags(t *testing.T) {
	for _, args := range [][]string{{"doctor", "--role", "router"}, {"doctor", "--port", "70000"}, {"doctor", "extra"}} {
		if code, _, _ := as(t, 0, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
