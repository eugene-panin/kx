package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

func (a *app) add(src, client, name string, contexts []string, force bool) error {
	if !validName(client) {
		return fmt.Errorf("invalid client name %q", client)
	}
	if src != "-" {
		if abs, err := filepath.Abs(src); err == nil && abs == a.target {
			return fmt.Errorf("%s is the file kx generates; use `kx import-current` to take over its contexts", src)
		}
	}
	if err := a.prepare(); err != nil {
		return err
	}
	cfg, err := readKubeconfig(src, a.stdin)
	if err != nil {
		return err
	}
	if len(contexts) == 0 {
		contexts = contextNames(cfg)
	}
	switch {
	case len(contexts) == 0:
		return fmt.Errorf("%s has no contexts", src)
	case name != "" && len(contexts) > 1:
		return fmt.Errorf("--name needs exactly one context, %s has %d; pick one with --context", src, len(contexts))
	}

	type item struct {
		r   ref
		cfg *api.Config
	}
	var items []item
	seen := map[ref]string{}
	for _, ctx := range contexts {
		cluster := name
		if cluster == "" {
			cluster = sanitize(ctx)
		}
		if !validName(cluster) {
			return fmt.Errorf("invalid cluster name %q", cluster)
		}
		r := ref{client: client, cluster: cluster}
		if prev, dup := seen[r]; dup {
			return fmt.Errorf("contexts %q and %q both map to %s; add them one by one with --context and --name", prev, ctx, r)
		}
		seen[r] = ctx
		if !force && a.store.exists(r) {
			return fmt.Errorf("%s already exists; use --force to overwrite or --name to pick another name", r)
		}
		one, err := extract(cfg, ctx, r.String())
		if err != nil {
			return err
		}
		items = append(items, item{r, one})
	}
	for _, it := range items {
		if err := a.store.put(it.r, it.cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "added %s\t%s\n", it.r, it.cfg.Clusters[it.r.String()].Server)
	}
	return a.build(false, nil)
}

func (a *app) importCurrent(client string) error {
	if !validName(client) {
		return fmt.Errorf("invalid client name %q", client)
	}
	if err := a.syncNamespaces(); err != nil {
		return err
	}
	names, err := a.unmanaged()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(a.stdout, "nothing to import")
		return nil
	}
	cfg, err := readKubeconfig(a.target, nil)
	if err != nil {
		return err
	}
	taken := map[ref]bool{}
	renames := map[string]string{}
	for _, ctx := range names {
		r := ref{client: client, cluster: sanitize(ctx)}
		for i, base := 2, r.cluster; taken[r] || a.store.exists(r); i++ {
			r.cluster = fmt.Sprintf("%s-%d", base, i)
		}
		taken[r] = true
		one, err := extract(cfg, ctx, r.String())
		if err != nil {
			return err
		}
		if err := a.store.put(r, one); err != nil {
			return err
		}
		renames[ctx] = r.String()
		fmt.Fprintf(a.stdout, "imported %s as %s\n", ctx, r)
	}
	// The originals now live in the store under new names; let the build drop them.
	st, err := a.store.loadState()
	if err != nil {
		return err
	}
	st.Generated = append(st.Generated, names...)
	if err := a.store.saveState(st); err != nil {
		return err
	}
	return a.build(false, renames)
}

type listRow struct {
	Client    string `json:"client"`
	Cluster   string `json:"cluster"`
	Context   string `json:"context"`
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
	Enabled   bool   `json:"enabled"`
	Current   bool   `json:"current"`
}

func (a *app) list(client string, asJSON bool) error {
	refs, err := a.store.clusters()
	if err != nil {
		return err
	}
	st, err := a.store.loadState()
	if err != nil {
		return err
	}
	cur, err := a.loadTarget()
	if err != nil {
		cur = api.NewConfig()
	}
	rows := []listRow{}
	for _, r := range refs {
		if client != "" && r.client != client {
			continue
		}
		cfg, err := a.store.get(r)
		if err != nil {
			return err
		}
		row := listRow{
			Client:  r.client,
			Cluster: r.cluster,
			Context: r.String(),
			Enabled: st.enabled(r),
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
	if client != "" && len(rows) == 0 {
		return fmt.Errorf("%s: not found", client)
	}

	if asJSON {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.stderr, "no clusters yet: kx add <kubeconfig> -c <client>")
		return nil
	}

	o := newOutput(a.stdout)
	if !o.tty {
		// One line per cluster with the client on it, so grep keeps working.
		cols := []column{{}, {title: "CLIENT"}, {title: "CLUSTER"}, {title: "SERVER"}, {title: "NAMESPACE"}, {title: "STATE"}}
		var out []row
		for _, r := range rows {
			mark, state := " ", "on"
			if r.Current {
				mark = "*"
			}
			if !r.Enabled {
				state = "off"
			}
			out = append(out, row{cells: []cell{{text: mark}, {text: r.Client}, {text: r.Cluster}, {text: r.Server}, {text: r.Namespace}, {text: state}}})
		}
		return o.table(cols, out)
	}

	cols := []column{{}, {title: "CLUSTER", shrink: 12}, {title: "SERVER", shrink: 16, trim: true}, {title: "NAMESPACE", drop: 1}, {title: "STATE"}}
	var out []row
	last := ""
	for _, r := range rows {
		if r.Client != last {
			last = r.Client
			title := o.paint(o.title, r.Client)
			if !st.enabled(ref{client: r.Client}) {
				title += " " + o.paint(o.dim, "off")
			}
			out = append(out, row{title: title})
		}
		base := o.plain
		if !r.Enabled {
			base = o.dim
		}
		mark, name, state := cell{"  ", base}, cell{r.Cluster, base}, cell{"on", o.ok}
		if r.Current {
			mark, name.style = cell{" *", o.ok}, o.bold
		}
		if !r.Enabled {
			state = cell{"off", o.dim}
		}
		out = append(out, row{cells: []cell{mark, name, {r.Server, base}, {r.Namespace, base}, state}})
	}
	return o.table(cols, out)
}

func (a *app) toggle(args []string, on bool) error {
	if err := a.prepare(); err != nil {
		return err
	}
	all, err := a.store.clusters()
	if err != nil {
		return err
	}
	st, err := a.store.loadState()
	if err != nil {
		return err
	}
	word := "off"
	if on {
		word = "on"
	}
	for _, arg := range args {
		if _, err := a.store.expand([]string{arg}); err != nil {
			return err
		}
		r, _ := parseRef(arg)
		if on {
			st.enable(r, all)
		} else {
			st.disable(r)
		}
		fmt.Fprintf(a.stdout, "%s: %s\n", r, word)
	}
	if err := a.store.saveState(st); err != nil {
		return err
	}
	return a.build(false, nil)
}

func (a *app) remove(args []string, yes bool) error {
	if err := a.prepare(); err != nil {
		return err
	}
	refs, err := a.store.expand(args)
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
			fmt.Fprintln(a.stderr, "aborted")
			return nil
		}
	}
	for _, r := range refs {
		if err := a.store.remove(r); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "removed %s\n", r)
	}
	if err := a.pruneState(); err != nil {
		return err
	}
	return a.build(false, nil)
}

func (a *app) move(fromArg, toArg string) error {
	from, err := parseRef(fromArg)
	if err != nil {
		return err
	}
	to, err := parseRef(toArg)
	if err != nil {
		return err
	}
	if err := a.prepare(); err != nil {
		return err
	}
	srcs, err := a.store.expand([]string{fromArg})
	if err != nil {
		return err
	}

	type pair struct{ from, to ref }
	var moves []pair
	switch {
	case from.cluster == "" && to.cluster != "":
		return fmt.Errorf("cannot move client %s into cluster %s", from, to)
	case from.cluster == "":
		for _, s := range srcs {
			moves = append(moves, pair{s, ref{client: to.client, cluster: s.cluster}})
		}
	case to.cluster == "":
		moves = []pair{{from, ref{client: to.client, cluster: from.cluster}}}
	default:
		moves = []pair{{from, to}}
	}
	for _, m := range moves {
		if m.from == m.to {
			return fmt.Errorf("%s: source and destination are the same", m.from)
		}
		if a.store.exists(m.to) {
			return fmt.Errorf("%s already exists", m.to)
		}
	}

	st, err := a.store.loadState()
	if err != nil {
		return err
	}
	renames := map[string]string{}
	for _, m := range moves {
		renames[m.from.String()] = m.to.String()
		wasOff := !st.enabled(m.from)
		if err := a.store.move(m.from, m.to); err != nil {
			return err
		}
		if wasOff {
			st.Disabled = append(st.Disabled, m.to.String())
		}
		fmt.Fprintf(a.stdout, "moved %s -> %s\n", m.from, m.to)
	}
	if err := a.store.saveState(st); err != nil {
		return err
	}
	if err := a.pruneState(); err != nil {
		return err
	}
	return a.build(false, renames)
}

func (a *app) export(args []string) error {
	refs, err := a.store.expand(args)
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
	_, err = a.stdout.Write(data)
	return err
}

func (a *app) pruneState() error {
	refs, err := a.store.clusters()
	if err != nil {
		return err
	}
	st, err := a.store.loadState()
	if err != nil {
		return err
	}
	st.prune(refs)
	return a.store.saveState(st)
}

func (a *app) confirm(question string) (bool, error) {
	fmt.Fprintf(a.stderr, "%s [y/N] ", question)
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(a.stderr)
		return false, fmt.Errorf("no confirmation; pass -y to skip it")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
