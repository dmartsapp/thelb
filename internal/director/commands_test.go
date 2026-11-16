package director

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// TestCommandsRunNothingTheyAreGiven runs the script Commands prints from a
// state full of shell syntax - which the leader would refuse to record, but
// which an older version or a member that misbehaves could leave there - with
// a stand-in for thelb: nothing in the state may run, and every value must
// reach thelb as the one word it was.
func TestCommandsRunNothingTheyAreGiven(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	owned := filepath.Join(dir, "owned")
	evil := "$(touch " + owned + ")"
	st := &cluster.State{
		Cluster: &cluster.Info{Name: "prod\ntouch " + owned, Lease: 10},
		Nodes:   map[string]*cluster.Node{"lb-1": {}, "x'; touch " + owned + "; '": {}, "y\ntouch " + owned: {}},
		Pools: map[string]*cluster.Pool{
			"p": {Name: "p;touch " + owned, First: "10.0.0.20", Bits: 24, Count: 10, Interface: "`touch " + owned + "`"},
		},
		Services: map[string]*cluster.Service{
			"s": {Name: evil, Pool: "p", VIP: "10.0.0.20 && touch " + owned, Proto: "tcp", Port: 80, Scheduler: "wrr|touch " + owned},
		},
		Assignments: map[string]*cluster.Assignment{
			"s/b": {Service: evil, Node: "b'\"$HOME", Proto: "tcp", Port: 8080, Weight: 3},
		},
	}
	script := Commands(st, "lb-1\ntouch "+owned, time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	args := filepath.Join(dir, "args")
	run := "thelb() { printf '%s\\n' \"$@\" >>" + args + "; printf '=\\n' >>" + args + "; }\n" + script
	if out, err := exec.Command(sh, "-c", run).CombinedOutput(); err != nil {
		t.Fatalf("the script fails: %v\n%s\n%s", err, out, script)
	}
	if _, err := os.Stat(owned); err == nil {
		t.Fatalf("the script ran a command from the state:\n%s", script)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"admin\npool\nadd\np;touch " + owned + "\n--vip\n10.0.0.20/24\n--vippool-count\n10\n--interface\n`touch " + owned + "`\n=\n",
		"admin\nservice\nadd\n" + evil + "\n--pool\np\n--vip\n10.0.0.20 && touch " + owned + "\n",
		"--scheduler\nwrr|touch " + owned + "\n",
		"admin\nassign\n" + evil + "\nb'\"$HOME:tcp/8080\n--weight\nb'\"$HOME=3\n=\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("thelb was not given %q; it got:\n%s", want, got)
		}
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "touch") {
			t.Errorf("a line of the script begins with a value of the state: %q", line)
		}
	}
}
