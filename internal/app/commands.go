package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eugene-panin/kx/internal/probe"
	"github.com/eugene-panin/kx/internal/store"
	"github.com/eugene-panin/kx/internal/table"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// Add imports the contexts of a kubeconfig (a file, or "-" for stdin) under
// client and returns the clusters it added. Every context is validated before
// anything is written.
func (a *App) Add(src, client, name string, contexts []string, force bool) ([]store.Ref, error) {
	if !store.ValidName(client) {
		return nil, fmt.Errorf("%w: client %q, want [A-Za-z0-9._-]", store.ErrInvalidName, client)
	}
	if src != "-" {
		// a.Target has its symlinks resolved; resolve src the same way, or a
		// dotfiles-managed ~/.kube/config slips past this guard.
		abs, err := filepath.Abs(src)
		if err == nil {
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				abs = real
			}
		}
		if abs == a.Target {
			return nil, fmt.Errorf("%s is the file kx generates; use `kx import-current` to take over its contexts", src)
		}
	}
	if err := a.prepare(); err != nil {
		return nil, err
	}
	if src == "-" && table.IsTerminal(a.Stdin) {
		if a.NoInput {
			return nil, &UsageError{Err: errors.New("--no-input: nothing to read, stdin is a terminal"), Hint: "pipe the kubeconfig in: kx add - -c <client> < file"}
		}
		fmt.Fprintln(a.Stderr, "Paste the kubeconfig, then press Ctrl-D (Ctrl-C to cancel).")
	}
	cfg, err := store.ReadKubeconfig(src, a.Stdin)
	if err != nil {
		return nil, err
	}
	// Not before the paste: another kx would wait while the user copies.
	unlock, err := a.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if len(contexts) == 0 {
		contexts = store.ContextNames(cfg)
	}
	switch {
	case len(contexts) == 0:
		return nil, fmt.Errorf("%s has no contexts", src)
	case name != "" && len(contexts) > 1:
		return nil, fmt.Errorf("--name needs exactly one context, %s has %d; pick one with --context", src, len(contexts))
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
			return nil, fmt.Errorf("%w: cluster %q, want [A-Za-z0-9._-]", store.ErrInvalidName, cluster)
		}
		r := store.Ref{Client: client, Cluster: cluster}
		if prev, dup := seen[r]; dup {
			return nil, fmt.Errorf("contexts %q and %q both map to %s; add them one by one with --context and --name", prev, ctx, r)
		}
		seen[r] = ctx
		if !force && a.Store.Exists(r) {
			return nil, fmt.Errorf("%s already exists; use --force to overwrite or --name to pick another name", r)
		}
		one, err := store.Extract(cfg, ctx, r.String())
		if err != nil {
			return nil, err
		}
		if err := store.Validate(one, r.String()); err != nil {
			return nil, fmt.Errorf("context %q: %w", ctx, err)
		}
		items = append(items, item{r, one})
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return nil, err
	}
	var added, off []store.Ref
	for _, it := range items {
		verb := "added"
		if old, err := a.Store.Get(it.r); err == nil {
			verb = "replaced"
			// A namespace set with kx ns outlives the new credentials unless
			// the new kubeconfig picks one itself.
			name := it.r.String()
			if oc, nc := old.Contexts[name], it.cfg.Contexts[name]; oc != nil && nc != nil && nc.Namespace == "" {
				nc.Namespace = oc.Namespace
			}
		}
		if err := a.Store.Put(it.r, it.cfg); err != nil {
			return added, err
		}
		added = append(added, it.r)
		if !st.Enabled(it.r) {
			off = append(off, it.r)
		}
		a.say("%s %s  %s", verb, it.r, it.cfg.Clusters[it.r.String()].Server)
	}
	if err := a.build(false, nil); err != nil {
		return added, err
	}
	for _, r := range off {
		a.say("%s stays off (kx on %s turns it on)", r, r)
	}
	return added, nil
}

// ImportCurrent takes over the contexts of the kubeconfig kx does not manage,
// under client; with dryRun it only says what it would import.
func (a *App) ImportCurrent(client string, dryRun bool) error {
	if !store.ValidName(client) {
		return fmt.Errorf("%w: client %q, want [A-Za-z0-9._-]", store.ErrInvalidName, client)
	}
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	names, err := a.Unmanaged()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		a.say("nothing to import")
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
		if dryRun {
			fmt.Fprintf(a.Stdout, "would import %s as %s\n", ctx, r)
			continue
		}
		one, err := store.Extract(cfg, ctx, r.String())
		if err != nil {
			return err
		}
		if err := a.Store.Put(r, one); err != nil {
			return err
		}
		renames[ctx] = r.String()
		a.say("imported %s as %s", ctx, r)
	}
	if dryRun {
		return nil
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
	return a.build(false, renames)
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

	o := a.Output(a.Stdout)
	if !o.TTY {
		// One line per cluster with the client on it, so grep keeps working.
		cols := []table.Column{{Title: "CURRENT"}, {Title: "CLIENT"}, {Title: "CLUSTER"}, {Title: "SERVER"}, {Title: "NAMESPACE"}, {Title: "VERSION"}, {Title: "STATE"}}
		var out []table.Row
		for _, r := range rows {
			// "-" rather than blank, or awk's fields shift on the current row.
			mark, state := "-", "on"
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
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	all, err := a.Store.Clusters()
	if err != nil {
		return err
	}
	// Check every argument before changing anything, so one typo at the end
	// doesn't leave half the request applied and reported as done.
	if _, err := a.Store.Expand(args); err != nil {
		return err
	}
	before, err := a.Store.LoadState()
	if err != nil {
		return err
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return err
	}
	refs := make([]store.Ref, len(args))
	for i, arg := range args {
		refs[i], _ = store.ParseRef(arg)
		if on {
			st.Enable(refs[i], all)
		} else {
			st.Disable(refs[i])
		}
	}
	if err := a.Store.SaveState(st); err != nil {
		return err
	}
	if err := a.build(false, nil); err != nil {
		// The target didn't change, so neither should the state: otherwise
		// the next build would quietly finish this one.
		if rerr := a.Store.SaveState(before); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return err
	}
	for _, r := range refs {
		a.say("%s", toggled(r, on, all, st))
	}
	return nil
}

// toggled says what kx on/off did to r. For a client it counts the clusters
// and names the ones left off, which a client entry alone doesn't show.
func toggled(r store.Ref, on bool, all []store.Ref, st *store.State) string {
	word := "off"
	if on {
		word = "on"
	}
	if r.Cluster != "" {
		return r.String() + ": " + word
	}
	var mine, off []string
	for _, c := range all {
		if c.Client != r.Client {
			continue
		}
		mine = append(mine, c.Cluster)
		if !st.Enabled(c) {
			off = append(off, c.Cluster)
		}
	}
	if !on || len(off) == 0 {
		return fmt.Sprintf("%s: %s (%s)", r, word, plural(len(mine), "cluster"))
	}
	return fmt.Sprintf("%s: on, %d of %d clusters; still off: %s", r, len(mine)-len(off), len(mine), strings.Join(off, ", "))
}

// Remove deletes clusters from the store. dryRun lists them and changes
// nothing; otherwise it asks first unless yes.
func (a *App) Remove(args []string, yes, dryRun bool) error {
	if err := a.prepare(); err != nil {
		return err
	}
	refs, err := a.Store.Expand(args)
	if err != nil {
		return err
	}
	if dryRun {
		for _, r := range refs {
			fmt.Fprintf(a.Stdout, "would remove %s\n", r)
		}
		return nil
	}
	// The store holds the only copy of a cluster's credentials once it's off,
	// so every removal is confirmed, one cluster or many.
	if !yes {
		names := make([]string, len(refs))
		for i, r := range refs {
			names[i] = r.String()
		}
		question := "remove " + names[0] + "?"
		if len(refs) > 1 {
			question = fmt.Sprintf("remove %d clusters: %s?", len(refs), strings.Join(names, ", "))
		}
		if err := a.confirm(question, "-y"); err != nil {
			return err
		}
	}
	// Not before the question: another kx would wait for the answer.
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	for _, r := range refs {
		if err := a.Store.Remove(r); err != nil {
			return err
		}
		a.say("removed %s", r)
	}
	if err := a.pruneState(); err != nil {
		return err
	}
	return a.build(false, nil)
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
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
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
	var turnedOff []store.Ref
	for _, m := range moves {
		renames[m.from.String()] = m.to.String()
		wasOff := !st.Enabled(m.from)
		if err := a.Store.Move(m.from, m.to); err != nil {
			return err
		}
		if wasOff {
			st.Disabled = append(st.Disabled, m.to.String())
		} else if !st.Enabled(m.to) {
			turnedOff = append(turnedOff, m.to)
		}
		a.say("moved %s -> %s", m.from, m.to)
	}
	if err := a.Store.SaveState(st); err != nil {
		return err
	}
	if err := a.pruneState(); err != nil {
		return err
	}
	if err := a.build(false, renames); err != nil {
		return err
	}
	for _, r := range turnedOff {
		a.say("%s is off now: client %s is off (kx on %s turns it back on)", r, r.Client, r)
	}
	return nil
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
		if _, err := a.Store.Expand([]string{r.Client}); err != nil {
			return fmt.Errorf("no client or cluster named %s", r)
		}
		return fmt.Errorf("%s is a client; use takes a cluster: %s/<cluster>", r, r)
	}
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
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
		if err := a.build(false, nil); err != nil {
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
	a.say("using %s", r)
	return nil
}

// Namespace prints or sets the default namespace of a context: the current
// one, or the cluster named in args. Like Use it rewrites just that field of
// the target, and the store too, so the namespace survives off/on.
func (a *App) Namespace(args []string) error {
	cfg, err := a.loadTarget()
	if err != nil {
		return err
	}
	var refArg, ns string
	switch {
	case len(args) == 0:
		ctx := cfg.Contexts[cfg.CurrentContext]
		if ctx == nil {
			return errors.New("no current context; pick one with kx use <client/cluster>")
		}
		fmt.Fprintln(a.Stdout, orDefault(ctx.Namespace))
		return nil
	case len(args) == 1 && strings.Contains(args[0], "/"):
		// A namespace never has a slash, so this names a cluster to show.
		r, err := store.ParseRef(args[0])
		if err != nil {
			return err
		}
		if r.Cluster == "" || !a.Store.Exists(r) {
			return fmt.Errorf("%s: not a cluster kx manages", args[0])
		}
		if ctx := cfg.Contexts[r.String()]; ctx != nil {
			fmt.Fprintln(a.Stdout, orDefault(ctx.Namespace))
			return nil
		}
		stored, err := a.Store.Get(r)
		if err != nil {
			return err
		}
		ns := ""
		if ctx := stored.Contexts[r.String()]; ctx != nil {
			ns = ctx.Namespace
		}
		fmt.Fprintln(a.Stdout, orDefault(ns))
		return nil
	case len(args) == 1:
		refArg, ns = cfg.CurrentContext, args[0]
		if refArg == "" {
			return errors.New("no current context; name the cluster: kx ns <client/cluster> <namespace>")
		}
	default:
		refArg, ns = args[0], args[1]
	}
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return &UsageError{Err: fmt.Errorf("invalid namespace %q: want lowercase letters, digits and '-', at most 63", ns)}
	}
	r, err := store.ParseRef(refArg)
	if err != nil {
		return err
	}
	if r.Cluster == "" || !a.Store.Exists(r) {
		return fmt.Errorf("%s: not a cluster kx manages", refArg)
	}
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	// Read again under the lock: another kx may have rebuilt it meanwhile.
	if cfg, err = a.loadTarget(); err != nil {
		return err
	}

	stored, err := a.Store.Get(r)
	if err != nil {
		return err
	}
	if ctx := stored.Contexts[r.String()]; ctx != nil && ctx.Namespace != ns {
		ctx.Namespace = ns
		if err := a.Store.Put(r, stored); err != nil {
			return err
		}
	}
	// A cluster that is off is not in the target; the store is enough then.
	if ctx := cfg.Contexts[r.String()]; ctx != nil && ctx.Namespace != ns {
		ctx.Namespace = ns
		data, err := clientcmd.Write(*cfg)
		if err != nil {
			return err
		}
		if err := store.WriteFile(a.Target, data); err != nil {
			return err
		}
	}
	a.say("%s: namespace %s", r, ns)
	return nil
}

func orDefault(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
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

// confirm asks on the terminal and returns nil on yes. Without a terminal it
// doesn't ask at all: a question nobody can answer only hangs scripts.
func (a *App) confirm(question, flag string) error {
	switch {
	case a.NoInput:
		return &UsageError{Err: errors.New("needs confirmation and --no-input is set"), Hint: "pass " + flag + " to go ahead without asking"}
	case !table.IsTerminal(a.Stdin):
		return &UsageError{Err: errors.New("needs confirmation and stdin is not a terminal"), Hint: "pass " + flag + " to go ahead without asking"}
	}
	fmt.Fprintf(a.Stderr, "%s [y/N] ", question)
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(a.Stderr)
		return &UsageError{Err: errors.New("no answer"), Hint: "pass " + flag + " to go ahead without asking"}
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	fmt.Fprintln(a.Stderr, "aborted")
	return ExitError(1)
}
