package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/eugene-panin/kx/internal/app"
	"github.com/eugene-panin/kx/internal/probe"
	"github.com/eugene-panin/kx/internal/store"
	"github.com/eugene-panin/kx/internal/table"
	"github.com/eugene-panin/kx/internal/tui"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// version is set with -ldflags "-X main.version=..." in release builds.
var version = "dev"

// buildVersion falls back to the module version Go stamps into the binary:
// v0.1.0 for go install ...@v0.1.0, a pseudo-version for a local checkout.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

// Exit codes: 0 ok, 1 the command failed, 2 kx was called wrong (bad flag,
// argument or missing confirmation), anything else is passed through from
// `kx exec` or reported by `kx check`.
func main() {
	os.Exit(report(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr), os.Stderr))
}

func report(err error, stderr io.Writer) int {
	var (
		code  app.ExitError
		usage *app.UsageError
	)
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, &usage):
		fmt.Fprintln(stderr, "kx:", usage.Err)
		if usage.Hint != "" {
			fmt.Fprintln(stderr, usage.Hint)
		}
		return 2
	case errors.Is(err, store.ErrInvalidName):
		fmt.Fprintln(stderr, "kx:", err)
		return 2
	default:
		fmt.Fprintln(stderr, "kx:", err)
		return 1
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	a, err := app.New(stdin, stdout, stderr)
	if err != nil {
		return err
	}
	var syncErr error
	root := cli{a, &syncErr}.command()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(flagWarnings{stdout, stderr})
	root.SetErr(stderr)
	root.SetFlagErrorFunc(helpWins(args))
	cmd, err := root.ExecuteC()
	if err != nil && isCobraUsage(err) {
		return &app.UsageError{Err: err, Hint: "Run '" + cmd.CommandPath() + " --help' for usage."}
	}
	// The sync before the command failed. Say so, unless the command failed
	// on the same broken file and says it already, or build --force just
	// replaced that file.
	if force, _ := cmd.Flags().GetBool("force"); syncErr != nil && !(cmd.Name() == "build" && force && err == nil) &&
		(err == nil || !strings.Contains(err.Error(), syncErr.Error())) {
		fmt.Fprintln(stderr, "kx: sync with", a.TargetName()+":", syncErr)
	}
	return err
}

// flagWarnings is stdout for cobra, except for pflag's deprecation warnings:
// cobra prints those through the same writer as help, and they belong on
// stderr [B4].
type flagWarnings struct{ stdout, stderr io.Writer }

func (w flagWarnings) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("Flag shorthand -")) || bytes.HasPrefix(p, []byte("Flag --")) {
		return w.stderr.Write(p)
	}
	return w.stdout.Write(p)
}

// helpWins turns a flag error into help when -h or --help is on the line, so
// `kx add --bogus -h` shows help instead of complaining about --bogus.
func helpWins(args []string) func(*cobra.Command, error) error {
	return func(c *cobra.Command, err error) error {
		for _, a := range args {
			if a == "--" {
				break
			}
			if a == "-h" || a == "--help" {
				return pflag.ErrHelp
			}
		}
		return &app.UsageError{Err: err, Hint: "Run '" + c.CommandPath() + " --help' for usage."}
	}
}

// isCobraUsage recognizes the argument errors cobra reports as plain errors.
func isCobraUsage(err error) bool {
	msg := err.Error()
	for _, p := range []string{"unknown command", "accepts ", "requires ", "required flag", "invalid argument"} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

// cli wires App into cobra commands.
type cli struct {
	*app.App
	syncErr *error // a failed sync before the command, reported after it
}

// usageTemplate is cobra's default with the examples moved to the top: people
// look for an example first [H7].
const usageTemplate = `{{if .HasExample}}Examples:
{{.Example}}

{{end}}Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more about a command.{{end}}
`

// needArgs is cobra.MinimumNArgs (or ExactArgs with exact) that answers a bare
// command with a short help instead of "accepts 1 arg(s), received 0" [H2].
func needArgs(n int, exact bool) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			var b strings.Builder
			fmt.Fprintf(&b, "%s\n\nUsage:\n  %s\n", c.Short, c.UseLine())
			if c.Example != "" {
				fmt.Fprintf(&b, "\nExamples:\n%s\n", c.Example)
			}
			fmt.Fprintf(&b, "\nRun '%s --help' for all flags.", c.CommandPath())
			return &app.UsageError{Err: errors.New(c.Name() + ": missing arguments"), Hint: b.String()}
		case len(args) < n, exact && len(args) > n:
			return &app.UsageError{Err: fmt.Errorf("%s takes %d arguments, got %d", c.Name(), n, len(args)), Hint: "Run '" + c.CommandPath() + " --help' for usage."}
		}
		return nil
	}
}

func positive(d time.Duration) error {
	if d <= 0 {
		return &app.UsageError{Err: fmt.Errorf("--timeout must be positive, got %s", d), Hint: "e.g. --timeout 10s"}
	}
	return nil
}

// unsorted guards cobra's global sorting switch: tests build commands in parallel.
var unsorted sync.Once

func (a cli) command() *cobra.Command {
	// Commands are listed in the order added, grouped, common ones first [H8].
	unsorted.Do(func() { cobra.EnableCommandSorting = false })
	root := &cobra.Command{
		Use:   "kx",
		Short: "Manage kubeconfig clusters grouped by client",
		Long: `kx keeps every cluster as a separate kubeconfig, grouped by client, and
builds ~/.kube/config from the ones that are on. Clusters are addressed as
<client>/<cluster>. In a terminal, plain kx opens the interactive view.

Files: clusters in ~/.config/kx, backups in ~/.local/state/kx, check results
in ~/.cache/kx (XDG_CONFIG_HOME, XDG_STATE_HOME and XDG_CACHE_HOME move them),
or all of it in $KX_HOME. The kubeconfig it builds is $KX_KUBECONFIG or
~/.kube/config. NO_COLOR, KX_NO_COLOR and FORCE_COLOR are honored.

Exit codes: 0 done, 1 failed, 2 called wrong (bad flag or argument, or a
question kx can't ask without a terminal).

Docs:   https://github.com/eugene-panin/kx
Issues: https://github.com/eugene-panin/kx/issues`,
		Example: `  kx                          open the interactive view
  kx add - -c acme            add a kubeconfig pasted on stdin
  kx ls                       list clusters
  kx off globex               hide a client's clusters for now
  kx check                    are they all reachable?`,
		Version:       buildVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// No Args validator: cobra's default for a root with subcommands is
		// what turns `kx lss` into "unknown command ... Did you mean ls?".
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.NoInput || !table.IsTerminal(a.Stdin) || !table.IsTerminal(a.Stdout) {
				return cmd.Help()
			}
			return tui.Run(a.App)
		},
		// Hand edits of ~/.kube/config are taken in once, before any command
		// looks at the store; a dry run promises to write nothing, so it
		// skips this. A broken file must not block kx build --force, so a
		// failure here is only a warning, given after the command.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			switch cmd.Name() {
			case "completion", "help", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
				return
			}
			a.MoveOldFiles()
			if dry, _ := cmd.Flags().GetBool("dry-run"); dry || cmd.Name() == "sync" {
				return
			}
			*a.syncErr = a.SyncAndReport()
		},
	}
	root.SetUsageTemplate(usageTemplate)
	// --version without -v [A5]; -v keeps working for now, with a warning [F3].
	root.Flags().BoolP("version", "v", false, "print the version")
	root.Flags().MarkShorthandDeprecated("version", "use --version")
	g := root.PersistentFlags()
	g.BoolVar(&a.NoColor, "no-color", false, "no colors (also NO_COLOR, KX_NO_COLOR)")
	g.BoolVar(&a.NoInput, "no-input", false, "never ask; fail when a question would be needed")
	g.BoolVarP(&a.Quiet, "quiet", "q", false, "print only results, not status lines")
	root.AddGroup(
		&cobra.Group{ID: "clusters", Title: "Clusters:"},
		&cobra.Group{ID: "context", Title: "Current context:"},
		&cobra.Group{ID: "health", Title: "Health and scripts:"},
		&cobra.Group{ID: "maint", Title: "Maintenance:"},
	)

	var (
		client, name string
		contexts     []string
		force        bool
		checkAdded   bool
		addTimeout   time.Duration
	)
	add := &cobra.Command{
		Use:     "add <kubeconfig|-> -c <client>",
		Short:   "Import clusters from a kubeconfig file or stdin",
		GroupID: "clusters",
		Long: `Import every context of a kubeconfig (or only the --context ones) as
<client>/<name>. Nothing is written unless every context has a server address
and credentials. Certificates referenced as files are copied in, so the source
file can be deleted afterwards. "-" reads stdin.`,
		Example: `  kx add ~/Downloads/kubeconfig.yaml -c acme
  kx add - -c acme --name prod --check
  kx add big.yaml -c acme --context ctx-a --context ctx-b`,
		Args: needArgs(1, true),
		RunE: func(cmd *cobra.Command, args []string) error {
			if checkAdded {
				if err := positive(addTimeout); err != nil {
					return err
				}
			}
			added, err := a.Add(args[0], client, name, contexts, force)
			if err != nil || !checkAdded {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return a.CheckAdded(ctx, added, addTimeout)
		},
	}
	add.Flags().StringVarP(&client, "client", "c", "", "client the clusters belong to (required)")
	add.Flags().StringVarP(&name, "name", "n", "", "cluster name (single context only)")
	// -n is the conventional --dry-run; it leaves --name in v1 [A5, F3].
	add.Flags().MarkShorthandDeprecated("name", "use --name")
	add.Flags().StringArrayVar(&contexts, "context", nil, "import only this context (repeatable)")
	add.Flags().BoolVarP(&force, "force", "f", false, "overwrite existing clusters")
	add.Flags().BoolVar(&checkAdded, "check", false, "check the added clusters right away; exit 1 if a check fails")
	add.Flags().DurationVarP(&addTimeout, "timeout", "t", probe.Timeout, "per-cluster timeout for --check")
	add.MarkFlagRequired("client")

	var asJSON bool
	ls := &cobra.Command{
		Use:     "ls [client]",
		Aliases: []string{"list"},
		Short:   "List clusters",
		GroupID: "clusters",
		Example: `  kx ls
  kx ls acme
  kx ls --json | jq -r '.[] | select(.enabled) | .context'`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeClients,
		RunE: func(cmd *cobra.Command, args []string) error {
			var c string
			if len(args) == 1 {
				c = args[0]
			}
			return a.List(c, asJSON)
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "print JSON (no credentials included)")

	on := &cobra.Command{
		Use:     "on <client|client/cluster>...",
		Short:   "Turn clusters back on",
		GroupID: "clusters",
		Example: `  kx on globex
  kx on acme/stage`,
		Args:              needArgs(1, false),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Toggle(args, true)
		},
	}
	off := &cobra.Command{
		Use:     "off <client|client/cluster>...",
		Short:   "Turn clusters off without removing them",
		GroupID: "clusters",
		Long: `Turn clusters off: they stay in kx but leave ~/.kube/config, so Lens, k9s and
kubectl stop seeing them. kx on brings them back.`,
		Example: `  kx off globex
  kx off acme/stage acme/dev`,
		Args:              needArgs(1, false),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Toggle(args, false)
		},
	}

	var yes, rmDry bool
	rm := &cobra.Command{
		Use:     "rm <client|client/cluster>...",
		Aliases: []string{"remove"},
		Short:   "Remove clusters for good",
		GroupID: "clusters",
		Long: `Remove clusters from kx. Asks first, every time: for a cluster that is off,
kx holds the only copy of its credentials. Without a terminal there is no
question; pass -y, or kx exits 2. kx export keeps a copy.`,
		Example: `  kx rm globex
  kx rm acme/stage --dry-run
  kx rm acme/stage -y`,
		Args:              needArgs(1, false),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Remove(args, yes, rmDry)
		},
	}
	rm.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	rm.Flags().BoolVar(&rmDry, "dry-run", false, "list what would be removed and change nothing")

	mv := &cobra.Command{
		Use:     "mv <from> <to>",
		Short:   "Rename a cluster or client, or move a cluster to another client",
		GroupID: "clusters",
		Example: `  kx mv acme/kubernetes-admin-kubernetes acme/prod
  kx mv unsorted/main globex
  kx mv acme acme-corp`,
		Args:              needArgs(2, true),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Move(args[0], args[1])
		},
	}

	var importClient string
	var importDry bool
	importCurrent := &cobra.Command{
		Use:     "import-current",
		Short:   "Take over contexts in ~/.kube/config that kx does not manage yet",
		GroupID: "clusters",
		Long: `Take over every context in ~/.kube/config that kx doesn't manage, e.g. one
written by aws eks update-kubeconfig, under client -c. Sort them out with mv.`,
		Example: `  kx import-current --dry-run
  kx import-current
  kx import-current -c acme`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.ImportCurrent(importClient, importDry)
		},
	}
	importCurrent.Flags().StringVarP(&importClient, "client", "c", "unsorted", "client to put the contexts under")
	importCurrent.Flags().BoolVar(&importDry, "dry-run", false, "list what would be imported and change nothing")

	use := &cobra.Command{
		Use:     "use [client/cluster]",
		Short:   "Set current-context, or print it without arguments",
		GroupID: "context",
		Long: `Set current-context: the cluster kubectl, helm, k9s and friends talk to when
no --context is given. Without arguments prints the current one.`,
		Example: `  kx use acme/prod
  kx use`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var r string
			if len(args) == 1 {
				r = args[0]
			}
			return a.Use(r)
		},
	}

	ns := &cobra.Command{
		Use:     "ns [client/cluster] [namespace]",
		Short:   "Show or set the default namespace of a context",
		GroupID: "context",
		Long: `Without arguments prints the namespace of the current context. With a
namespace sets it on the current context; with a cluster and a namespace sets
it on that cluster, even one that is off. The namespace is not looked up in
the cluster, so this works offline.`,
		Example: `  kx ns
  kx ns monitoring
  kx ns acme/prod monitoring`,
		Args:              cobra.MaximumNArgs(2),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Namespace(args)
		},
	}

	var (
		checkAll  bool
		checkJSON bool
		timeout   time.Duration
	)
	check := &cobra.Command{
		Use:     "check [client|client/cluster]...",
		Short:   "Check that clusters answer and credentials work",
		GroupID: "health",
		Long: `Check reachability, server version, health and credentials of clusters in
parallel. Without arguments checks the clusters that are on. Exits 1 if any
check fails, so it works from cron. Expiry (days left) is read from client
certificates and JWT tokens; "!" marks under 30 days. Errors are listed below
the table; --json has full details. Ctrl-C stops it with exit code 130.`,
		Example: `  kx check
  kx check acme --timeout 30s
  kx check --all --json | jq '.[] | select(.status != "ok")'`,
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := positive(timeout); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return a.Check(ctx, args, checkAll, timeout, checkJSON)
		},
	}
	check.Flags().BoolVarP(&checkAll, "all", "a", false, "include clusters that are off")
	check.Flags().BoolVar(&checkJSON, "json", false, "print JSON")
	check.Flags().DurationVarP(&timeout, "timeout", "t", probe.Timeout, "per-cluster timeout")

	export := &cobra.Command{
		Use:     "export <client|client/cluster>...",
		Short:   "Print a self-contained kubeconfig for the given clusters",
		GroupID: "health",
		Example: `  kx export acme/prod > acme-prod.yaml
  kx export acme globex/main > handover.yaml`,
		Args:              needArgs(1, false),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Export(args)
		},
	}

	execCmd := &cobra.Command{
		Use:     "exec <client|client/cluster>... -- <command> [args...]",
		Short:   "Run a command that sees only the given clusters",
		GroupID: "health",
		Long: `Run a command with KUBECONFIG pointing at a temporary kubeconfig holding only
the given clusters (disabled ones included); the first one is current.
KX_SCOPE is set to the requested clusters, e.g. for a shell prompt. Exits with
the command's exit code.

This guards against mistakes, not a hostile process: ~/.kube/config and the
store stay readable.`,
		Example: `  kx exec acme -- $SHELL
  kx exec acme/stage -- k9s
  kx exec acme globex/main -- kubectl get nodes`,
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			hint := "e.g. kx exec acme -- k9s"
			switch {
			case dash < 0 && len(args) == 0:
				return needArgs(1, false)(cmd, args)
			case dash < 0:
				return &app.UsageError{Err: errors.New("separate clusters from the command with --"), Hint: hint}
			case dash == 0:
				return &app.UsageError{Err: errors.New("no clusters given before --"), Hint: hint}
			case dash == len(args):
				return &app.UsageError{Err: errors.New("no command given after --"), Hint: hint}
			}
			return a.Exec(args[:dash], args[dash:])
		},
	}

	ui := &cobra.Command{
		Use:     "ui",
		Short:   "Interactive mode (also what plain `kx` runs in a terminal)",
		GroupID: "maint",
		Example: `  kx ui`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.NoInput || !table.IsTerminal(a.Stdin) || !table.IsTerminal(a.Stdout) {
				return &app.UsageError{Err: errors.New("kx ui needs a terminal and input"), Hint: "the commands in kx --help do everything it does"}
			}
			return tui.Run(a.App)
		},
	}

	sync := &cobra.Command{
		Use:     "sync",
		Short:   "Take hand edits of ~/.kube/config into kx",
		GroupID: "maint",
		Long: `Every kx command does this first; this runs it on its own.

A context kx manages that was edited by hand (server, certificates, token,
namespace) replaces the stored copy. One that was deleted by hand is turned
off, not removed: kx on brings it back. Contexts kx doesn't manage are left
alone; take them over with kx import-current.`,
		Example: `  kx sync`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			notes, err := a.Sync()
			for _, n := range notes {
				fmt.Fprintln(a.Stdout, n)
			}
			if err == nil && len(notes) == 0 && !a.Quiet {
				fmt.Fprintln(a.Stdout, "nothing to sync")
			}
			return err
		},
	}

	var buildForce, buildDry bool
	build := &cobra.Command{
		Use:     "build",
		Short:   "Regenerate ~/.kube/config from the clusters that are on",
		GroupID: "maint",
		Long: `Regenerate ~/.kube/config. Every command that changes something does this on
its own. --force drops contexts kx doesn't manage, after saving a backup.`,
		Example: `  kx build
  kx build --force --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Rebuild(buildForce, buildDry)
		},
	}
	build.Flags().BoolVarP(&buildForce, "force", "f", false, "drop contexts kx does not manage")
	build.Flags().BoolVar(&buildDry, "dry-run", false, "say what would be written and dropped, write nothing")

	root.AddCommand(add, ls, on, off, rm, mv, importCurrent, use, ns, check, export, execCmd, ui, sync, build)

	// cobra's own help command prints "Unknown help topic" to stdout and
	// exits 0; a typo there is a usage error like anywhere else [H10].
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(c *cobra.Command, args []string) error {
			cmd, _, err := root.Find(args)
			if cmd == nil || err != nil || len(args) > 0 && cmd == root {
				hint := "Run 'kx --help' for the list of commands."
				if s := root.SuggestionsFor(args[len(args)-1]); len(s) > 0 {
					hint = "Did you mean this?\n\t" + strings.Join(s, "\n\t")
				}
				return &app.UsageError{Err: fmt.Errorf("unknown help topic %q", strings.Join(args, " ")), Hint: hint}
			}
			return cmd.Help()
		},
	})
	return root
}

func (a cli) completeRefs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	refs, err := a.Store.Clusters()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []string
	last := ""
	for _, r := range refs {
		if r.Client != last && strings.HasPrefix(r.Client, toComplete) {
			out = append(out, r.Client)
		}
		last = r.Client
		if strings.HasPrefix(r.String(), toComplete) {
			out = append(out, r.String())
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func (a cli) completeClients(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	refs, err := a.Store.Clusters()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []string
	for _, r := range refs {
		if (len(out) == 0 || out[len(out)-1] != r.Client) && strings.HasPrefix(r.Client, toComplete) {
			out = append(out, r.Client)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
