package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// How the help looks. cobra makes each page from the command's definition -
// its Long text, usage, examples, subcommands and flags - so a flag the parser
// reads is a flag the help shows. On top of cobra's layout:
//   - a command's flags may be in sections ("Cluster flags:", ...), named by
//     the annotation annSection on each flag;
//   - flags a command inherits are under the command that gives them: "Global
//     flags:" for the root's, "Flags of thelb generate k8s:" for that one's;
//   - a page may end with a link to its documentation (annDocs).

const docsURL = "https://dmartsapp.github.io/thelb/"

// Annotations theLB puts on commands and flags.
const (
	annDocs    = "thelb_docs"    // a command's: the URL of its documentation
	annGroup   = "thelb_group"   // a command's: it only groups subcommands
	annSection = "thelb_section" // a flag's: the section of the help it is in
)

// helpWidth is where the flags' descriptions wrap.
const helpWidth = 80

const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if and .HasAvailableSubCommands (not (isGroup .))}}
  {{.CommandPath}} <command> [flags]{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Commands:{{range $cmds}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding}} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding}} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Other commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding}} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}{{flagSections .}}{{end}}{{if .HasAvailableInheritedFlags}}{{inheritedSections .}}{{end}}{{if .HasAvailableSubCommands}}

Run '{{.CommandPath}} <command> --help' for more about a command.{{end}}{{with docs .}}

Documentation: {{.}}{{end}}
`

func init() {
	cobra.EnableCommandSorting = false // commands in the order they are added: the order that reads well
	cobra.AddTemplateFunc("isGroup", func(c *cobra.Command) bool { return c.Annotations[annGroup] != "" })
	cobra.AddTemplateFunc("flagSections", flagSections)
	cobra.AddTemplateFunc("inheritedSections", inheritedSections)
	cobra.AddTemplateFunc("docs", func(c *cobra.Command) string { return c.Annotations[annDocs] })
}

// layout sets every command's help in theLB's layout, and keeps its flags in
// the order they are defined - the order the help means to show them in.
func layout(c *cobra.Command) {
	c.SetUsageTemplate(usageTemplate)
	c.DisableFlagsInUseLine = true // each Use line says where its flags go
	c.Flags().SortFlags = false
	c.PersistentFlags().SortFlags = false
	for _, s := range c.Commands() {
		layout(s)
	}
}

// section puts flags in a section of their command's help.
func section(c *cobra.Command, title string, flags ...string) {
	for _, f := range flags {
		if err := c.Flags().SetAnnotation(f, annSection, []string{title}); err != nil {
			panic(err) // a flag that does not exist: a mistake in this file
		}
	}
}

// required marks flags a command cannot run without - cobra refuses to run
// it without them, and completion offers them first.
func required(c *cobra.Command, flags ...string) {
	for _, f := range flags {
		if err := c.MarkFlagRequired(f); err != nil {
			panic(err)
		}
	}
}

// entries is how the help shows each of a command's flags - its own and those
// it inherits - by name: rendered together, so they share one column.
func entries(c *cobra.Command) map[string]string {
	all := pflag.NewFlagSet(c.Name(), pflag.ContinueOnError)
	all.SortFlags = false
	all.AddFlagSet(c.LocalFlags())
	all.AddFlagSet(c.InheritedFlags())
	out := map[string]string{}
	name := ""
	for _, line := range strings.Split(all.FlagUsagesWrapped(helpWidth), "\n") {
		if m := flagLineRE.FindStringSubmatch(line); m != nil {
			name = m[1]
		}
		if name != "" && strings.TrimSpace(line) != "" {
			out[name] += strings.TrimRight(line, " ") + "\n"
		}
	}
	return out
}

// flagLineRE is the first line of a flag's entry: its name.
var flagLineRE = regexp.MustCompile(`^\s+(?:-[a-zA-Z0-9], )?--([a-z0-9][a-z0-9-]*)\b`)

// flagSections is a command's own flags, in their sections - "Flags:" for
// those in none.
func flagSections(c *cobra.Command) string {
	text := entries(c)
	var order []string
	sections := map[string]*strings.Builder{}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		title := "Flags"
		if v := f.Annotations[annSection]; len(v) > 0 {
			title = v[0] + " flags"
		}
		if sections[title] == nil {
			sections[title] = &strings.Builder{}
			order = append(order, title)
		}
		sections[title].WriteString(text[f.Name])
	})
	var b strings.Builder
	for _, title := range order {
		fmt.Fprintf(&b, "\n\n%s:\n%s", title, strings.TrimRight(sections[title].String(), "\n"))
	}
	return b.String()
}

// inheritedSections is the flags a command inherits, under the command that
// gives them: the nearest first, the global flags last.
func inheritedSections(c *cobra.Command) string {
	text := entries(c)
	inherited := c.InheritedFlags()
	shown := map[string]bool{} // a flag given again nearer shadows the one further up
	var b strings.Builder
	for p := c.Parent(); p != nil; p = p.Parent() {
		var set strings.Builder
		p.PersistentFlags().VisitAll(func(f *pflag.Flag) { // in the order defined
			if !f.Hidden && !shown[f.Name] && inherited.Lookup(f.Name) != nil && c.LocalFlags().Lookup(f.Name) == nil {
				shown[f.Name] = true
				set.WriteString(text[f.Name])
			}
		})
		switch {
		case set.Len() == 0:
		case p == c.Root():
			fmt.Fprintf(&b, "\n\nGlobal flags:\n%s", strings.TrimRight(set.String(), "\n"))
		default:
			fmt.Fprintf(&b, "\n\nFlags of %s:\n%s", p.CommandPath(), strings.TrimRight(set.String(), "\n"))
		}
	}
	return b.String()
}
