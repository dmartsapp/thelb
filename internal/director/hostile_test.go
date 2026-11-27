package director

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/safe"
	"github.com/dmartsapp/thelb/internal/secret"
)

// The local socket takes JSON from root - from thelb, which checks what it
// sends, or from anything else root runs. Whatever arrives, the leader answers
// and goes on leading: a request that once stopped it (service.remove without
// a name) is the reason for these tests.

// hostileValues are the values each field of a change is tried with.
var (
	hostileNames = [][]string{nil, {}, {""}, {"-"}, {strings.Repeat("a", 4096)}, {"../../etc/passwd"}, {"a\nb"}, {"\x00"}, {"web", "web", "web"},
		{"\u202eweb"}, {"WEB"}, {"lb-1"}, func() []string {
			many := make([]string, 5000)
			for i := range many {
				many[i] = fmt.Sprintf("n%d", i)
			}
			return many
		}()}
	hostileVIPs    = []string{"", "x", "0.0.0.0/0", "::/0", "10.0.0.50/33", "255.255.255.255/32", "224.0.0.1/24", "fe80::1%eth0/64", "10.0.0.50", "10.0.0.50/24", "127.0.0.1/8", "::ffff:10.0.0.50/120", strings.Repeat("1", 5000)}
	hostileInts    = []int{math.MinInt, -1, 0, 1, 2, 3, 4096, 4097, 65535, 65536, 3601, math.MaxInt32, math.MaxInt}
	hostileStrings = []string{"", "x", "tcp", "udp", "sctp", "icmp", "TCP", "wrr", "mh", "nat", "tunnel", "direct", "lo", "eth0;id", "../../x", strings.Repeat("z", 5000), "a\x1b[2Jb"}
	hostileOffers  = [][]string{nil, {""}, {":"}, {"b1:"}, {":web"}, {"b1:tcp/0"}, {"b1:tcp/99999"}, {"lb-1:web"}, {"b1:web", "b1:web"}, {"b1:udp/53"}}
	hostileWeights = []map[string]int{nil, {"": -1}, {"b1": math.MaxInt}, {"b1": math.MinInt}, {"b1": 0}, {"\n": 1}}
	changeOps      = []string{"approve", "refuse", "remove", "pool.add", "pool.remove", "service.add", "service.remove", "assign", "unassign", "lease", "", "nope", "APPROVE", "service.add\n", "kube.alloc", "cluster.create"}
)

// hostileChanges are changes with one hostile field each, on every operation,
// and a few hundred with every field hostile at once.
func hostileChanges() []api.Change {
	var out []api.Change
	// Each operation's base is one the leader takes, on the cluster
	// hostileSetup makes: so each hostile field is the one it refuses for.
	bases := map[string]api.Change{
		"pool.add":       {Op: "pool.add", Pool: "spare", VIP: "10.0.0.200/24", Count: 4},
		"pool.remove":    {Op: "pool.remove", Pool: "spare"},
		"service.add":    {Op: "service.add", Names: []string{"svc"}, Proto: "tcp", Port: 80, Pool: "main"},
		"service.remove": {Op: "service.remove", Names: []string{"svc"}},
		"lease":          {Op: "lease", Seconds: 6},
		"assign":         {Op: "assign", Names: []string{"web"}, Offers: []string{"b1:web"}},
		"unassign":       {Op: "unassign", Names: []string{"web", "b1"}},
	}
	for _, op := range changeOps {
		base, ok := bases[op]
		if !ok {
			base = api.Change{Op: op, Names: []string{"web"}, VIP: "10.0.0.50/24", Count: 4, Seconds: 6, Pool: "main", Proto: "tcp", Port: 80, Offers: []string{"b1:web"}}
		}
		for _, v := range hostileNames {
			c := base
			c.Names = v
			out = append(out, c)
		}
		for _, v := range hostileVIPs {
			c := base
			c.VIP = v
			out = append(out, c)
		}
		for _, v := range hostileInts {
			c := base
			c.Count, c.Seconds, c.Port = v, v, v
			out = append(out, c)
		}
		for _, v := range hostileStrings {
			c := base
			c.Proto, c.Scheduler, c.Method, c.Interface, c.Pool = v, v, v, v, v
			out = append(out, c)
		}
		for _, v := range hostileOffers {
			c := base
			c.Offers = v
			out = append(out, c)
		}
		for _, v := range hostileWeights {
			c := base
			c.Weights = v
			out = append(out, c)
		}
	}
	r := rand.New(rand.NewSource(1))
	pick := func(n int) int { return r.Intn(n) }
	for i := 0; i < 300; i++ {
		out = append(out, api.Change{
			Op: changeOps[pick(len(changeOps))], Names: hostileNames[pick(len(hostileNames))], VIP: hostileVIPs[pick(len(hostileVIPs))],
			Count: hostileInts[pick(len(hostileInts))], Seconds: hostileInts[pick(len(hostileInts))], Port: hostileInts[pick(len(hostileInts))],
			Interface: hostileStrings[pick(len(hostileStrings))], Default: pick(2) == 1, Pool: hostileStrings[pick(len(hostileStrings))],
			Proto: hostileStrings[pick(len(hostileStrings))], Scheduler: hostileStrings[pick(len(hostileStrings))], Method: hostileStrings[pick(len(hostileStrings))],
			Offers: hostileOffers[pick(len(hostileOffers))], Weights: hostileWeights[pick(len(hostileWeights))],
		})
	}
	return out
}

// hostileSetup gives the cluster a pool, main, and a service, web, in it.
func hostileSetup(t *testing.T, w *world) {
	t.Helper()
	for _, c := range []api.Change{{Op: "pool.add", Pool: "main", VIP: "10.0.0.50/24", Count: 64, Default: true}, {Op: "service.add", Names: []string{"web"}, Proto: "tcp", Port: 80}} {
		c := c
		if resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpChange, Change: &c}); err != nil || resp.Error != "" {
			t.Fatalf("%s: %v %s", c.Op, err, resp.Error)
		}
	}
}

// tryChange sends c, as JSON, and undoes it if the leader took it - a pool or a
// service added is removed again - so each change meets the same cluster.
func tryChange(t *testing.T, w *world, c api.Change) (localsock.Response, error) {
	t.Helper()
	req, _ := json.Marshal(localsock.Request{V: localsock.Version, Op: localsock.OpChange, Change: &c})
	resp, err := rawCall(t, w.run, append(req, '\n'))
	if err == nil && resp.OK {
		var undo *api.Change
		switch c.Op {
		case "pool.add":
			undo = &api.Change{Op: "pool.remove", Pool: c.Pool}
		case "service.add":
			undo = &api.Change{Op: "service.remove", Names: c.Names}
		case "lease":
			undo = &api.Change{Op: "lease", Seconds: 6}
		}
		if undo != nil {
			b, _ := json.Marshal(localsock.Request{V: localsock.Version, Op: localsock.OpChange, Change: undo})
			if r, err := rawCall(t, w.run, append(b, '\n')); err != nil || !r.OK {
				t.Fatalf("undoing %q: %v %s", c.Op, err, r.Error)
			}
		}
	}
	return resp, err
}

// rawCall sends bytes to the local socket as they are, and reads one answer.
func rawCall(t *testing.T, dir string, b []byte) (localsock.Response, error) {
	t.Helper()
	c, err := net.DialTimeout("unix", filepath.Join(dir, localsock.SocketName), 2*time.Second)
	if err != nil {
		return localsock.Response{}, err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	_, _ = c.Write(b) // a request past the socket's limit is cut off: its answer still comes
	if uc, ok := c.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	var resp localsock.Response
	err = json.NewDecoder(bufio.NewReader(c)).Decode(&resp)
	return resp, err
}

// stillLeads checks that the director is up and leading, and that its state
// still renders.
func stillLeads(t *testing.T, w *world, after string) {
	t.Helper()
	select {
	case err := <-w.errc:
		w.errc <- err
		t.Fatalf("after %s, the director stopped: %v\n%s", after, err, w.logs)
	default:
	}
	st := w.status(t)
	if st.Cluster == nil || st.Cluster.Leader != w.cfg.Name {
		t.Fatalf("after %s, it no longer leads: %+v\n%s", after, st.Cluster, w.logs)
	}
	if _, err := st.Text(time.Now()); err != nil {
		t.Fatalf("after %s, its status does not render: %v", after, err)
	}
	if strings.Contains(w.logs.String(), "panic") {
		t.Fatalf("after %s, a panic in the log:\n%s", after, w.logs)
	}
}

func TestHostileAdminChangesNeverStopTheLeader(t *testing.T) {
	w := single(t)
	w.plane = &recPlane{}
	w.start(t)
	eventually(t, "it leads", func() bool { return strings.Contains(w.logs.String(), "OK control VIP taken") })
	hostileSetup(t, w)

	changes := hostileChanges()
	answered := 0
	for i, c := range changes {
		c := c
		resp, err := tryChange(t, w, c)
		if err != nil {
			t.Fatalf("change %d (%q): no answer: %v", i, c.Op, err)
		}
		if resp.OK || resp.Error != "" {
			answered++
		}
		if i%200 == 0 {
			stillLeads(t, w, fmt.Sprintf("change %d (%q)", i, c.Op))
		}
	}
	stillLeads(t, w, fmt.Sprintf("%d hostile changes", len(changes)))
	if answered != len(changes) {
		t.Errorf("%d of %d changes got neither an OK nor an error", len(changes)-answered, len(changes))
	}
	// Whatever went in, generate commands still writes a script.
	if resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpGenerate, What: "commands"}); err != nil || resp.Error != "" {
		t.Errorf("generate commands after the hostile changes: %v %s", err, resp.Error)
	}
}

func TestHostileLocalRequestsNeverStopTheLeader(t *testing.T) {
	w := single(t)
	w.plane = &recPlane{}
	w.start(t)
	eventually(t, "it leads", func() bool { return strings.Contains(w.logs.String(), "OK control VIP taken") })

	v := localsock.Version
	for _, raw := range []string{
		"", "\n", "{", "null\n", "[]\n", `"x"` + "\n", "{}\n",
		fmt.Sprintf(`{"v":%d}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"change"}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"change","change":null}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"change","change":{}}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"change","change":{"op":"service.remove"}}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"change","change":{"op":"approve","names":[null]}}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"change","change":{"op":"assign","names":["web"],"offers":[]}}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"change","change":{"op":"lease","seconds":1e300}}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"offer"}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"offer","offer":{"op":"add","offer":{"name":"x","proto":"tcp","port":80}}}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"secret.show","role":"root"}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"secret.show","role":"../director"}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"generate","what":"../../etc/shadow"}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"status","brief":"yes"}`+"\n", v),
		fmt.Sprintf(`{"v":%d,"op":"nope"}`+"\n", v),
		`{"v":-1,"op":"status"}` + "\n",
		`{"v":99999999999999999999,"op":"status"}` + "\n",
		strings.Repeat("[", 100000) + "\n",
		fmt.Sprintf(`{"v":%d,"op":"status","x":"`, v) + strings.Repeat("a", 70<<10) + `"}` + "\n",
		fmt.Sprintf(`{"v":%d,"op":"status"}`, v), // no newline: the caller closes
		fmt.Sprintf(`{"v":%d,"op":"status"}`+"\n"+`{"v":%d,"op":"change","change":{"op":"lease","seconds":5}}`+"\n", v, v),
	} {
		resp, err := rawCall(t, w.run, []byte(raw))
		if err != nil && !strings.Contains(err.Error(), "EOF") {
			t.Errorf("%.60q: %v", raw, err)
		}
		_ = resp
		stillLeads(t, w, fmt.Sprintf("%.60q", raw))
	}
	// A secret is never shown for a role that is not one.
	for _, role := range []string{"", "root", "../director", "Director"} {
		if resp, _ := localsock.Call(w.run, localsock.Request{Op: localsock.OpSecretShow, Role: role}); resp.Secret != "" {
			t.Errorf("the secret shown for the role %q", role)
		}
	}
}

// TestManyLocalCallersAtOnce: a hundred callers at once, some holding their
// connection open without a word. The leader answers the others, and goes on.
func TestManyLocalCallersAtOnce(t *testing.T) {
	w := single(t)
	w.start(t)
	eventually(t, "it leads", func() bool { return strings.Contains(w.logs.String(), "OK control VIP taken") })
	var held []net.Conn
	for i := 0; i < 100; i++ {
		c, err := net.Dial("unix", filepath.Join(w.run, localsock.SocketName))
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		held = append(held, c)
	}
	done := make(chan error, 50)
	for i := 0; i < 50; i++ {
		go func() {
			_, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpStatus})
			done <- err
		}()
	}
	for i := 0; i < 50; i++ {
		if err := <-done; err != nil {
			t.Errorf("a status asked beside 100 silent callers: %v", err)
		}
	}
	for _, c := range held {
		_ = c.Close()
	}
	stillLeads(t, w, "100 silent callers")
}

// TestHostileChangesTakeOnlyWhatIsValid: of the hostile changes, the leader
// takes only those a person could mean - each judged on the same cluster -
// and records nothing it should not.
func TestHostileChangesTakeOnlyWhatIsValid(t *testing.T) {
	w := single(t)
	w.plane = &recPlane{}
	w.start(t)
	eventually(t, "it leads", func() bool { return strings.Contains(w.logs.String(), "OK control VIP taken") })
	hostileSetup(t, w)
	name := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	in := func(s string, set ...string) bool { return slices.Contains(set, s) }
	took := map[string]int{}
	for _, c := range hostileChanges() {
		resp, err := tryChange(t, w, c)
		if err != nil || !resp.OK {
			continue
		}
		took[c.Op]++
		why := ""
		switch c.Op {
		case "lease":
			if c.Seconds < 3 || c.Seconds > 3600 {
				why = "a lease out of bounds"
			}
		case "pool.add":
			p, err := netip.ParsePrefix(c.VIP)
			if !name.MatchString(c.Pool) || err != nil || p.Addr().Is4In6() || c.Count < 1 || c.Count > 4096 || (c.Interface != "" && !safe.IfName(c.Interface)) {
				why = "a pool that is not one"
			}
		case "service.add":
			if len(c.Names) != 1 || !name.MatchString(c.Names[0]) || !in(c.Proto, "tcp", "udp", "sctp") || c.Port < 1 || c.Port > 65535 ||
				!in(c.Scheduler, "", "wrr", "rr", "wlc", "lc", "sh", "mh") || !in(c.Method, "", "direct", "tunnel") || (c.Pool != "" && !name.MatchString(c.Pool)) {
				why = "a service that is not one"
			}
		default:
			why = "nothing to " + c.Op + " on this cluster"
		}
		if why != "" {
			t.Errorf("took %s: %+v -> %+v", why, c, resp.Result)
		}
	}
	for _, op := range []string{"lease", "pool.add", "service.add"} {
		if took[op] == 0 {
			t.Errorf("took no %s at all: the bases are wrong, and the test proves nothing", op)
		}
	}
	// What the cluster holds afterwards is what hostileSetup made.
	st := w.status(t)
	if len(st.Pools) != 1 || len(st.Services) != 1 || st.Pools[0].Pool != "main" || st.Services[0].Service != "web" {
		t.Errorf("left behind: pools %+v services %+v", st.Pools, st.Services)
	}
}

// TestAnUnusableSecretFileStopsADirectorAtOnce: a director without --bootstrap
// finds nothing on the control VIP and waits for a cluster - but a secret file
// it could never use stops it before that wait, saying why.
func TestAnUnusableSecretFileStopsADirectorAtOnce(t *testing.T) {
	for _, c := range []struct {
		what string
		make func(path string) error
		says string
	}{
		{"a symbolic link", func(p string) error { return os.Symlink("/etc/hostname", p) }, "symbolic link"},
		{"a FIFO", func(p string) error { return syscall.Mkfifo(p, 0o600) }, "not a regular file"},
		{"a directory", func(p string) error { return os.Mkdir(p, 0o700) }, "not a regular file"},
		{"open to others", func(p string) error { return os.WriteFile(p, []byte("x"), 0o644) }, "can be read by group or others"},
		{"empty", func(p string) error { return os.WriteFile(p, nil, 0o600) }, "not a theLB secret"},
		{"not a secret", func(p string) error { return os.WriteFile(p, []byte("thelb1:nope\n"), 0o600) }, "not a theLB secret"},
	} {
		t.Run(c.what, func(t *testing.T) {
			w := single(t)
			w.cfg.Bootstrap = false
			if err := c.make(w.cfg.SecretFile); err != nil {
				t.Skip(err)
			}
			w.launch(t)
			select {
			case err := <-w.errc:
				w.errc <- err
				if err == nil || !strings.Contains(err.Error(), c.says) {
					t.Fatalf("%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the director waits for a cluster with a secret file that is %s\n%s", c.what, w.logs)
			}
		})
	}
	// And the backends' secret, missing, without --bootstrap: said at once.
	w := single(t)
	w.cfg.Bootstrap = false
	s, _ := secret.Generate()
	if err := os.WriteFile(w.cfg.SecretFile, []byte(s.Encode()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.launch(t)
	select {
	case err := <-w.errc:
		w.errc <- err
		if err == nil || !strings.Contains(err.Error(), "the backends' secret too") {
			t.Fatalf("%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the director waits for a cluster without the backends' secret")
	}
}

// TestRefusalsAreLoggedOnceAMinuteAnAddress: a flood of wrong secrets from one
// address is one log line a minute, saying how many were refused since; each
// address its own.
func TestRefusalsAreLoggedOnceAMinuteAnAddress(t *testing.T) {
	var r refusals
	a, b := netip.MustParseAddr("10.0.0.66"), netip.MustParseAddr("10.0.0.67")
	t0 := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	said := 0
	for i := 0; i < 500; i++ {
		if say, _ := r.note(a, t0.Add(time.Duration(i)*10*time.Millisecond)); say {
			said++
		}
	}
	if said != 1 {
		t.Errorf("500 refusals in 5 s logged %d times", said)
	}
	if say, _ := r.note(b, t0.Add(time.Second)); !say {
		t.Error("another address's first refusal was not logged")
	}
	say, more := r.note(a, t0.Add(61*time.Second))
	if !say || more != 499 {
		t.Errorf("a minute on: said %v, %d since the last line; want true, 499", say, more)
	}
	r.note(a, t0.Add(30*time.Minute))
	if len(r.last) != 1 {
		t.Errorf("quiet addresses are not forgotten: %d kept", len(r.last))
	}
}
