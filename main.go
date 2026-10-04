package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/eugene-panin/kx/internal/app"
	"github.com/eugene-panin/kx/internal/table"
	"github.com/eugene-panin/kx/internal/tui"
	"github.com/spf13/cobra"
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

func main() {
	err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	var code app.ExitError
	switch {
	case errors.As(err, &code):
		os.Exit(int(code))
	case err != nil:
		fmt.Fprintln(os.Stderr, "kx:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	a, err := app.New(stdin, stdout, stderr)
	if err != nil {
		return err
	}
	root := cli{a}.command()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.Execute()
}

// cli wires App into cobra commands.
type cli struct {
	*app.App
}

func (a cli) command() *cobra.Command {
	root := &cobra.Command{
		Use:   "kx",
		Short: "Manage kubeconfig clusters grouped by client",
		Long: `kx keeps every cluster as a separate kubeconfig under $KX_HOME (~/.config/kx)
and generates ~/.kube/config ($KX_KUBECONFIG) from the enabled ones.
Clusters are addressed as <client>/<cluster>.`,
		Version:       buildVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !table.IsTerminal(a.Stdin) || !table.IsTerminal(a.Stdout) {
				return cmd.Help()
			}
			return tui.Run(a.App)
		},
	}

	var (
		client, name string
		contexts     []string
		force        bool
	)
	add := &cobra.Command{
		Use:   "add <kubeconfig|-> -c <client>",
		Short: "Import clusters from a kubeconfig file or stdin",
		Example: `  kx add ~/Downloads/kubeconfig.yaml -c acme
  pbpaste | kx add - -c acme --name prod
  kx add big.yaml -c acme --context ctx-a --context ctx-b`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Add(args[0], client, name, contexts, force)
		},
	}
	add.Flags().StringVarP(&client, "client", "c", "", "client the clusters belong to (required)")
	add.Flags().StringVarP(&name, "name", "n", "", "cluster name (single context only)")
	add.Flags().StringArrayVar(&contexts, "context", nil, "import only this context (repeatable)")
	add.Flags().BoolVarP(&force, "force", "f", false, "overwrite existing clusters")
	add.MarkFlagRequired("client")

	var importClient string
	importCurrent := &cobra.Command{
		Use:   "import-current",
		Short: "Take over contexts in ~/.kube/config that kx does not manage yet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.ImportCurrent(importClient)
		},
	}
	importCurrent.Flags().StringVarP(&importClient, "client", "c", "unsorted", "client to put the contexts under")

	var asJSON bool
	ls := &cobra.Command{
		Use:               "ls [client]",
		Aliases:           []string{"list"},
		Short:             "List clusters",
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
		Use:               "on <client|client/cluster>...",
		Short:             "Enable clusters",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Toggle(args, true)
		},
	}
	off := &cobra.Command{
		Use:               "off <client|client/cluster>...",
		Short:             "Disable clusters without removing them",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Toggle(args, false)
		},
	}

	var yes bool
	rm := &cobra.Command{
		Use:               "rm <client|client/cluster>...",
		Short:             "Remove clusters",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Remove(args, yes)
		},
	}
	rm.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")

	mv := &cobra.Command{
		Use:   "mv <from> <to>",
		Short: "Rename a cluster or client, or move a cluster to another client",
		Example: `  kx mv acme/kubernetes-admin-kubernetes acme/prod
  kx mv unsorted/main globex
  kx mv acme acme-corp`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Move(args[0], args[1])
		},
	}

	use := &cobra.Command{
		Use:   "use [client/cluster]",
		Short: "Set current-context, or print it without arguments",
		Long: `Set current-context: the cluster kubectl, helm, k9s and friends talk to when
no --context is given. Without arguments prints the current one.`,
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

	export := &cobra.Command{
		Use:               "export <client|client/cluster>...",
		Short:             "Print a self-contained kubeconfig for the given clusters",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Export(args)
		},
	}

	execCmd := &cobra.Command{
		Use:   "exec <client|client/cluster>... -- <command> [args...]",
		Short: "Run a command that sees only the given clusters",
		Long: `Run a command with KUBECONFIG pointing at a temporary kubeconfig holding only
the given clusters (disabled ones included); the first one is current.
KX_SCOPE is set to the requested clusters, e.g. for a shell prompt.

This guards against mistakes, not a hostile process: ~/.kube/config and the
store stay readable.`,
		Example: `  kx exec acme -- $SHELL
  kx exec acme/stage -- k9s
  kx exec acme globex/main -- kubectl get nodes`,
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			switch {
			case dash < 0:
				return fmt.Errorf("separate clusters from the command with --, e.g. kx exec acme -- k9s")
			case dash == 0:
				return fmt.Errorf("no clusters given before --")
			case dash == len(args):
				return fmt.Errorf("no command given after --")
			}
			return a.Exec(args[:dash], args[dash:])
		},
	}

	var (
		checkAll  bool
		checkJSON bool
		timeout   time.Duration
	)
	check := &cobra.Command{
		Use:   "check [client|client/cluster]...",
		Short: "Check that clusters answer and credentials work",
		Long: `Check reachability, server version and credentials of clusters in parallel.
Without arguments checks the enabled clusters. Exits 1 if any check fails.
Expiry (days left) is read from client certificates and JWT tokens; "!" marks
under 30 days. Errors are listed below the table; --json has full details.`,
		ValidArgsFunction: a.completeRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Check(cmd.Context(), args, checkAll, timeout, checkJSON)
		},
	}
	check.Flags().BoolVarP(&checkAll, "all", "a", false, "include disabled clusters")
	check.Flags().BoolVar(&checkJSON, "json", false, "print JSON")
	check.Flags().DurationVarP(&timeout, "timeout", "t", 5*time.Second, "per-cluster timeout")

	ui := &cobra.Command{
		Use:   "ui",
		Short: "Interactive mode (also what plain `kx` runs in a terminal)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !table.IsTerminal(a.Stdin) || !table.IsTerminal(a.Stdout) {
				return errors.New("kx ui needs a terminal")
			}
			return tui.Run(a.App)
		},
	}

	var buildForce bool
	build := &cobra.Command{
		Use:   "build",
		Short: "Regenerate ~/.kube/config from enabled clusters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.Rebuild(buildForce)
		},
	}
	build.Flags().BoolVar(&buildForce, "force", false, "drop contexts kx does not manage")

	root.AddCommand(add, importCurrent, ls, use, on, off, rm, mv, export, execCmd, check, ui, build)
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
