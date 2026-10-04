package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eugene-panin/kx/internal/probe"
	"github.com/eugene-panin/kx/internal/store"
	"github.com/eugene-panin/kx/internal/table"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

func (a *App) Add(src, client, name string, contexts []string, force bool) error {
	if !store.ValidName(client) {
		return fmt.Errorf("invalid client name %q", client)
	}
	if src != "-" {
		if abs, err := filepath.Abs(src); err == nil && abs == a.Target {
			return fmt.Errorf("%s is the file kx generates; use `kx import-current` to take over its contexts", src)
		}
	}
	if err := a.prepare(); err != nil {
		return err
	}
	cfg, err := store.ReadKubeconfig(src, a.Stdin)
	if err != nil {
		return err
	}
	if len(contexts) == 0 {
		contexts = store.ContextNames(cfg)
	}
	switch {
	case len(contexts) == 0:
		return fmt.Errorf("%s has no contexts", src)
	case name != "" && len(contexts) > 1:
		return fmt.Errorf("--name needs exactly one context, %s has %d; pick one with --context", src, len(contexts))
	}

	type item struct {
		r   store.Ref
		cfg *api.Config
	}
	var items []item
	seen := map[store.Ref]string{}
	for _, ctx := range contexts {
		cluster := name
		if cluster == "" {
			cluster = store.Sanitize(ctx)
		}
		if !store.ValidName(cluster) {
			return fmt.Errorf("invalid cluster name %q", cluster)
		}
		r := store.Ref{Client: client, Cluster: cluster}
		if prev, dup := seen[r]; dup {
			return fmt.Errorf("contexts %q and %q both map to %s; add them one by one with --context and --name", prev, ctx, r)
		}
		seen[r] = ctx
		if !force && a.Store.Exists(r) {
			return fmt.Errorf("%s already exists; use --force to overwrite or --name to pick another name", r)
		}
		one, err := store.Extract(cfg, ctx, r.String())
		if err != nil {
			return err
		}
		items = append(items, item{r, one})
	}
	for _, it := range items {
		if err := a.Store.Put(it.r, it.cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "added %s\t%s\n", it.r, it.cfg.Clusters[it.r.String()].Server)
	}
	return a.Build(false, nil)
}

func (a *App) ImportCurrent(client string) error {
	if !store.ValidName(client) {
		return fmt.Errorf("invalid client name %q", client)
	}
	if err := a.syncNamespaces(); err != nil {
		return err
	}
	names, err := a.Unmanaged()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(a.Stdout, "nothing to import")
		return nil
	}
	cfg, err := store.ReadKubeconfig(a.Target, nil)
	if err != nil {
		return err
	}
	taken := map[store.Ref]bool{}
	renames := map[string]string{}
	for _, ctx := range names {
		r := store.Ref{Client: client, Cluster: store.Sanitize(ctx)}
		for i, base := 2, r.Cluster; taken[r] || a.Store.Exists(r); i++ {
			r.Cluster = fmt.Sprintf("%s-%d", base, i)
		}
		taken[r] = true
		one, err := store.Extract(cfg, ctx, r.String())
		if err != nil {
			return err
		}
		if err := a.Store.Put(r, one); err != nil {
			return err
		}
		renames[ctx] = r.String()
		fmt.Fprintf(a.Stdout, "imported %s as %s\n", ctx, r)
	}
	// The originals now live in the store under new names; let the build drop them.
	st, err := a.Store.LoadState()
	if err != nil {
		return err
	}
	st.Generated = append(st.Generated, names...)
	if err := a.Store.SaveState(st); err != nil {
		return err
	}
	return a.Build(false, renames)
}

type ListRow struct {
	Client    string `json:"client"`
	Cluster   string `json:"cluster"`
	Context   string `json:"context"`
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
	Version   string `json:"version,omitempty"` // as of the last check
	Enabled   bool   `json:"enabled"`
	Current   bool   `json:"current"`
}

// Rows describes every stored cluster as ls shows it.
func (a *App) Rows() ([]ListRow, *store.State, error) {
	refs, err := a.Store.Clusters()
	if err != nil {
		return nil, nil, err
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return nil, nil, err
	}
	cur, err := a.loadTarget()
	if err != nil {
		cur = api.NewConfig()
	}
	checks, err := a.LoadChecks()
	if err != nil {
		return nil, nil, err
	}
	rows := []ListRow{}
	for _, r := range refs {
		cfg, err := a.Store.Get(r)
		if err != nil {
			return nil, nil, err
		}
		row := ListRow{
			Client:  r.Client,
			Cluster: r.Cluster,
			Context: r.String(),
			Version: checks[r.String()].Version,
			Enabled: st.Enabled(r),
			Current: r.String() == cur.CurrentContext,
		}
		if ctx := cfg.Contexts[r.String()]; ctx != nil {
			row.Namespace = ctx.Namespace
			if cl := cfg.Clusters[ctx.Cluster]; cl != nil {
				row.Server = cl.Server
			}
		}
		// The target may hold a namespace switched since the last kx change.
		if ctx := cur.Contexts[r.String()]; ctx != nil {
			row.Namespace = ctx.Namespace
		}
		rows = append(rows, row)
	}
	return rows, st, nil
}

func (a *App) List(client string, asJSON bool) error {
	all, st, err := a.Rows()
	if err != nil {
		return err
	}
	rows := []ListRow{}
	for _, r := range all {
		if client == "" || r.Client == client {
			rows = append(rows, r)
		}
	}
	if client != "" && len(rows) == 0 {
		return fmt.Errorf("%s: not found", client)
	}

	if asJSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.Stderr, "no clusters yet: kx add <kubeconfig> -c <client>")
		return nil
	}

	o := table.New(a.Stdout)
	if !o.TTY {
		// One line per cluster with the client on it, so grep keeps working.
		cols := []table.Column{{}, {Title: "CLIENT"}, {Title: "CLUSTER"}, {Title: "SERVER"}, {Title: "NAMESPACE"}, {Title: "VERSION"}, {Title: "STATE"}}
		var out []table.Row
		for _, r := range rows {
			mark, state := " ", "on"
			if r.Current {
				mark = "*"
			}
			if !r.Enabled {
				state = "off"
			}
			out = append(out, table.Row{Cells: []table.Cell{{Text: mark}, {Text: r.Client}, {Text: r.Cluster}, {Text: r.Server}, {Text: r.Namespace}, {Text: r.Version}, {Text: state}}})
		}
		return o.Table(cols, out)
	}

	known, err := a.LoadChecks()
	if err != nil {
		return err
	}
	policy := probe.NewVersionPolicy(known)
	cols := []table.Column{
		{},
		{Title: "CLUSTER", Shrink: 12},
		{Title: "SERVER", Shrink: 16, Trim: true},
		{Title: "NAMESPACE", Drop: 1},
		{Title: "VERSION", Drop: 2},
		{Title: "STATE"},
	}
	var out []table.Row
	last := ""
	for _, r := range rows {
		if r.Client != last {
			last = r.Client
			title := o.Paint(o.Title, r.Client)
			if !st.Enabled(store.Ref{Client: r.Client}) {
				title += " " + o.Paint(o.Dim, "off")
			}
			out = append(out, table.Row{Title: title})
		}
		base := o.Plain
		if !r.Enabled {
			base = o.Dim
		}
		mark, name, state := table.Cell{Text: "  ", Style: base}, table.Cell{Text: r.Cluster, Style: base}, table.Cell{Text: "on", Style: o.OK}
		if r.Current {
			mark, name.Style = table.Cell{Text: " *", Style: o.OK}, o.Bold
		}
		if !r.Enabled {
			state = table.Cell{Text: "off", Style: o.Dim}
		}
		version := VersionCell(o, r.Version, policy)
		if !r.Enabled {
			version.Style = o.Dim
		}
		out = append(out, table.Row{Cells: []table.Cell{mark, name, {Text: r.Server, Style: base}, {Text: r.Namespace, Style: base}, version, state}})
	}
	return o.Table(cols, out)
}

func (a *App) Toggle(args []string, on bool) error {
	if err := a.prepare(); err != nil {
		return err
	}
	all, err := a.Store.Clusters()
	if err != nil {
		return err
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return err
	}
	word := "off"
	if on {
		word = "on"
	}
	for _, arg := range args {
		if _, err := a.Store.Expand([]string{arg}); err != nil {
			return err
		}
		r, _ := store.ParseRef(arg)
		if on {
			st.Enable(r, all)
		} else {
			st.Disable(r)
		}
		fmt.Fprintf(a.Stdout, "%s: %s\n", r, word)
	}
	if err := a.Store.SaveState(st); err != nil {
		return err
	}
	return a.Build(false, nil)
}

func (a *App) Remove(args []string, yes bool) error {
	if err := a.prepare(); err != nil {
		return err
	}
	refs, err := a.Store.Expand(args)
	if err != nil {
		return err
	}
	if len(refs) > 1 && !yes {
		names := make([]string, len(refs))
		for i, r := range refs {
			names[i] = r.String()
		}
		ok, err := a.confirm(fmt.Sprintf("remove %d clusters: %s?", len(refs), strings.Join(names, ", ")))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.Stderr, "aborted")
			return nil
		}
	}
	for _, r := range refs {
		if err := a.Store.Remove(r); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "removed %s\n", r)
	}
	if err := a.pruneState(); err != nil {
		return err
	}
	return a.Build(false, nil)
}

func (a *App) Move(fromArg, toArg string) error {
	from, err := store.ParseRef(fromArg)
	if err != nil {
		return err
	}
	to, err := store.ParseRef(toArg)
	if err != nil {
		return err
	}
	if err := a.prepare(); err != nil {
		return err
	}
	srcs, err := a.Store.Expand([]string{fromArg})
	if err != nil {
		return err
	}

	type pair struct{ from, to store.Ref }
	var moves []pair
	switch {
	case from.Cluster == "" && to.Cluster != "":
		return fmt.Errorf("cannot move client %s into cluster %s", from, to)
	case from.Cluster == "":
		for _, s := range srcs {
			moves = append(moves, pair{s, store.Ref{Client: to.Client, Cluster: s.Cluster}})
		}
	case to.Cluster == "":
		moves = []pair{{from, store.Ref{Client: to.Client, Cluster: from.Cluster}}}
	default:
		moves = []pair{{from, to}}
	}
	for _, m := range moves {
		if m.from == m.to {
			return fmt.Errorf("%s: source and destination are the same", m.from)
		}
		if a.Store.Exists(m.to) {
			return fmt.Errorf("%s already exists", m.to)
		}
	}

	st, err := a.Store.LoadState()
	if err != nil {
		return err
	}
	renames := map[string]string{}
	for _, m := range moves {
		renames[m.from.String()] = m.to.String()
		wasOff := !st.Enabled(m.from)
		if err := a.Store.Move(m.from, m.to); err != nil {
			return err
		}
		if wasOff {
			st.Disabled = append(st.Disabled, m.to.String())
		}
		fmt.Fprintf(a.Stdout, "moved %s -> %s\n", m.from, m.to)
	}
	if err := a.Store.SaveState(st); err != nil {
		return err
	}
	if err := a.pruneState(); err != nil {
		return err
	}
	return a.Build(false, renames)
}

func (a *App) Export(args []string) error {
	refs, err := a.Store.Expand(args)
	if err != nil {
		return err
	}
	cfg, err := a.bundle(refs)
	if err != nil {
		return err
	}
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return err
	}
	_, err = a.Stdout.Write(data)
	return err
}

// Use sets current-context. Only that field changes, so switching back and
// forth does not churn the backups the way a rebuild would.
func (a *App) Use(arg string) error {
	if arg == "" {
		cfg, err := a.loadTarget()
		if err != nil {
			return err
		}
		if cfg.CurrentContext == "" {
			fmt.Fprintln(a.Stderr, "no current context")
			return nil
		}
		fmt.Fprintln(a.Stdout, cfg.CurrentContext)
		return nil
	}
	r, err := store.ParseRef(arg)
	if err != nil {
		return err
	}
	if r.Cluster == "" {
		return fmt.Errorf("%s is a client; use takes a cluster: %s/<cluster>", r, r)
	}
	if !a.Store.Exists(r) {
		return fmt.Errorf("%s: not found", r)
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return err
	}
	if !st.Enabled(r) {
		return fmt.Errorf("%s is off; turn it on first", r)
	}
	cfg, err := a.loadTarget()
	if err != nil {
		return err
	}
	if cfg.Contexts[r.String()] == nil {
		// Enabled but missing: the target was edited by hand. Bring it back.
		if err := a.Build(false, nil); err != nil {
			return err
		}
		if cfg, err = a.loadTarget(); err != nil {
			return err
		}
	}
	if cfg.CurrentContext != r.String() {
		cfg.CurrentContext = r.String()
		data, err := clientcmd.Write(*cfg)
		if err != nil {
			return err
		}
		if err := store.WriteFile(a.Target, data); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.Stdout, "using %s\n", r)
	return nil
}

func (a *App) pruneState() error {
	refs, err := a.Store.Clusters()
	if err != nil {
		return err
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return err
	}
	st.Prune(refs)
	return a.Store.SaveState(st)
}

func (a *App) confirm(question string) (bool, error) {
	fmt.Fprintf(a.Stderr, "%s [y/N] ", question)
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(a.Stderr)
		return false, fmt.Errorf("no confirmation; pass -y to skip it")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
