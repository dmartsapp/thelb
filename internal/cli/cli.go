// Package cli is theLB's command line, built with cobra in the usual layout:
//
//	thelb [global flags] <command> [<subcommand> ...] [flags] [arguments]
//
// The commands are in groups - run a node, see and check, change the cluster,
// print files - and each has its subcommands and flags. Flags go where they
// apply: --debug on every command (global); generate k8s's --from-kubeconfig,
// --from-context and --yes on every form of it; the rest on their command.
// thelb --help lists the commands, thelb <command> --help shows one, and the
// help is made from the same definitions the parser reads, so the two cannot
// drift apart.
//
// Every command runs only as root (effective user ID 0); the help, --version
// and completion run for anyone. Exit statuses: 0 done, 1 failed, 2 used
// wrongly - a flag, an argument or a command theLB does not take.
//
// Where things are:
//
//	cli.go       Main, the root command, the groups, errors and exit statuses
//	layout.go    how the help looks: its template, flag sections, docs links
//	run.go       thelb director and thelb backend: their flags, checked into a
//	             director.Config or backend.Config - shared with generate
//	status.go    thelb status and thelb doctor
//	admin.go     thelb admin ...: changes, asked of the director on this node
//	offer.go     thelb offer ...: asked of the backend on this node
//	generate.go  thelb generate secret|commands|systemd|sysctl|modules
//	k8s*.go      thelb generate k8s ...
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dmartsapp/thelb/internal/backend"
	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/fingerprint"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/preflight"
	"github.com/dmartsapp/thelb/internal/safe"
	"github.com/dmartsapp/thelb/internal/version"
)

// What the command line reads the world through; tests replace these.
var (
	geteuid     = os.Geteuid
	goos        = runtime.GOOS
	runDir      = "/run/thelb" // each role has its own directory in it: director/, backend/
	lookup      = net.DefaultResolver.LookupNetIP
	localFor    = hostnet.For
	now         = time.Now
	hostInputs  = fingerprint.ReadHost
	executable  = os.Executable // generate systemd's ExecStart=
	runDirector = director.Run
	runBackend  = backend.Run
	// preflightDirector and preflightBackend check, before anything changes,
	// that this node has all theLB needs.
	preflightDirector = preflight.Director
	preflightBackend  = preflight.Backend
	logOutput         io.Writer // nil: standard error
	stdin             io.Reader = os.Stdin
	isTerminal                  = func() bool {
		fi, err := os.Stdin.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
)

// oneDashRE is a long flag given one dash: -vip, -version=x.
var oneDashRE = regexp.MustCompile(`^-([A-Za-z][A-Za-z0-9]*-?[A-Za-z0-9-]+(?:=.*)?)$`)

// usageError is a mistake in how theLB was called: exit status 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// notRoot is a command run by a user other than root: exit status 1, as
// before any flag is checked.
type notRoot struct{ cmd string }

func (e notRoot) Error() string {
	return e.cmd + " runs only as root (effective user ID 0); the help, --version and completion run for anyone"
}

// notYet is a command this build does not have.
func notYet(what string) error {
	return fmt.Errorf("%s is not in this build of theLB yet", what)
}

// app is one run of the command line: what every command writes to, and the
// global flags.
type app struct {
	ctx            context.Context
	stdout, stderr io.Writer
	debug          bool // --debug
	// ran is whether a command's own action started: an error before it is in
	// how theLB was called - an unknown command, flag or argument (exit 2).
	ran bool
}

// act is a command's action: it marks the run as started, so its errors are
// failures (exit 1) unless they are usage errors.
func (a *app) act(fn func(c *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(c *cobra.Command, args []string) error {
		a.ran = true
		return fn(c, args)
	}
}

// debugf writes a --debug line about what a command does, to standard error.
func (a *app) debugf(format string, v ...any) {
	if a.debug {
		_, _ = fmt.Fprintf(a.stderr, "debug: "+format+"\n", v...)
	}
}

// Main runs the command line and returns the exit status.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// Every argument is checked before any command reads it: none takes a
	// control character, and each may reach a unit, a script or a log line.
	for i, arg := range args {
		if err := safe.Arg(arg); err != nil {
			_, _ = fmt.Fprintf(stderr, "thelb: argument %d %s: theLB takes none like it\nRun 'thelb --help' for usage.\n", i+1, safe.Text(err.Error()))
			return 2
		}
	}
	// A long flag with one dash - which Go's flag package took, and so theLB
	// did before cobra - would read as short flags run together.
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if m := oneDashRE.FindStringSubmatch(arg); m != nil {
			_, _ = fmt.Fprintf(stderr, "thelb: %s: a long flag takes two dashes - --%s\nRun 'thelb --help' for usage.\n", safe.Text(arg), safe.Text(m[1]))
			return 2
		}
	}
	a := &app{ctx: ctx, stdout: stdout, stderr: stderr}
	root := a.root()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	if cmd == nil {
		cmd = root
	}
	// One line per line of the error - cobra's suggestions take several - each
	// shown without control characters, and named for its command once.
	lines := strings.Split(strings.TrimRight(err.Error(), "\n"), "\n")
	for i, l := range lines {
		l = safe.Text(strings.ReplaceAll(l, "\t", "  "))
		if i == 0 && !strings.HasPrefix(l, "thelb ") && !strings.HasPrefix(l, "thelb:") {
			l = cmd.CommandPath() + ": " + l
		}
		_, _ = fmt.Fprintln(stderr, l)
	}
	var u usageError
	var nr notRoot
	switch {
	case errors.As(err, &nr):
		return 1
	case errors.As(err, &u) || !a.ran:
		_, _ = fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
		return 2
	}
	return 1
}

// The groups of commands, as thelb --help lists them.
const (
	groupRun    = "run"
	groupLook   = "look"
	groupChange = "change"
	groupFiles  = "files"
	groupOther  = "other"
)

// root is the command line: the root command, every command under it, and
// the global flags.
func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "thelb",
		Short: "theLB - The Load Balancer: L4 load balancing for Linux, in one binary",
		Long: `theLB - The Load Balancer: L4 load balancing for Linux, in one binary.

Directors balance a VIP's connections over backends with the kernel's IPVS -
direct routing or IP-in-IP - and agree on who holds which VIP with Raft.
Backends offer their applications; directors assign them to services.`,
		Version:       version.String(),
		SilenceErrors: true, // Main prints them, with the exit status they get
		SilenceUsage:  true, // and points at the help, rather than printing it all
		Annotations:   map[string]string{annDocs: docsURL},
		// Every command runs as root; the help, --version and completion run
		// for anyone, and so does a command that only groups others.
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			if anyone(c) {
				return nil
			}
			if geteuid() != 0 {
				return notRoot{c.CommandPath()}
			}
			return nil
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usagef("%v", err) })
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentFlags().BoolP("help", "h", false, "Show the help")
	root.Flags().BoolP("version", "v", false, "Show the version, the commit, Go and the platform")
	root.PersistentFlags().BoolVar(&a.debug, "debug", false, "Say more: a director logs each connection it forwards, and the commands that ask the theLB running here say what they ask, on standard error")
	root.AddGroup(
		&cobra.Group{ID: groupRun, Title: "Run a node:"},
		&cobra.Group{ID: groupLook, Title: "See and check:"},
		&cobra.Group{ID: groupChange, Title: "Change the cluster:"},
		&cobra.Group{ID: groupFiles, Title: "Print files:"},
		&cobra.Group{ID: groupOther, Title: "Other commands:"},
	)
	addTo(root, groupRun, a.directorCmd(), a.backendCmd())
	addTo(root, groupLook, a.statusCmd(), a.doctorCmd())
	addTo(root, groupChange, a.adminCmd(), a.offerCmd())
	addTo(root, groupFiles, a.generateCmd())
	root.SetHelpCommand(a.helpCmd(root))
	root.SetHelpCommandGroupID(groupOther)
	root.SetCompletionCommandGroupID(groupOther)
	root.InitDefaultCompletionCmd()
	for _, c := range root.Commands() {
		if c.Name() == "completion" {
			c.Short = "Print the completion script of a shell: bash, zsh, fish or powershell"
		}
	}
	layout(root)
	return root
}

// addTo adds commands to parent, in one group of its help.
func addTo(parent *cobra.Command, group string, cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.GroupID = group
		parent.AddCommand(c)
	}
}

// anyone is whether a command runs for any user: the help, completion -
// cobra's own commands - and the commands that only group others.
func anyone(c *cobra.Command) bool {
	for p := c; p != nil; p = p.Parent() {
		switch p.Name() {
		case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return true
		}
	}
	return c.Annotations[annGroup] != ""
}

// group is a command that only groups subcommands: alone, it shows its help;
// with a word that is none of them, it says so - a usage error.
func (a *app) group(c *cobra.Command, subs ...*cobra.Command) *cobra.Command {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[annGroup] = "yes"
	c.Args = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		msg := fmt.Sprintf("%q is not a subcommand of %s", args[0], c.CommandPath())
		if c.SuggestionsMinimumDistance <= 0 {
			c.SuggestionsMinimumDistance = 2 // cobra's own, which it sets only for the root
		}
		if s := c.SuggestionsFor(args[0]); len(s) > 0 {
			msg += " - did you mean " + strings.Join(s, " or ") + "?"
		}
		return usageError{msg}
	}
	c.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
	c.AddCommand(subs...)
	return c
}

// helpCmd is thelb help <command words>: the help of that command, as
// --help shows it; words that are no command are a usage error.
func (a *app) helpCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "help [<command> ...]",
		Short: "Show the help of a command",
		Long:  "Show the help of a command - the same as thelb <command> --help.",
		ValidArgsFunction: func(c *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			target, _, err := root.Find(args)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []cobra.Completion
			for _, s := range target.Commands() {
				if s.IsAvailableCommand() {
					out = append(out, cobra.CompletionWithDesc(s.Name(), s.Short))
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(c *cobra.Command, args []string) error {
			target, rest, err := root.Find(args)
			if err != nil || len(rest) > 0 || (target == root && len(args) > 0) {
				return usagef("%q is not a theLB command", strings.Join(args, " "))
			}
			return target.Help()
		},
	}
}
