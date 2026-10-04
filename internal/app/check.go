package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/eugene-panin/kx/internal/probe"
	"github.com/eugene-panin/kx/internal/store"
	"github.com/eugene-panin/kx/internal/table"
)

func (a *App) Check(ctx context.Context, args []string, all bool, timeout time.Duration, asJSON bool) error {
	refs, err := a.checkTargets(args, all)
	if err != nil {
		return err
	}
	results, err := a.probeAll(ctx, refs, timeout)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else if err := a.printCheck(results); err != nil {
		return err
	}
	for _, res := range results {
		if res.Failed() {
			return ExitError(1)
		}
	}
	return nil
}

// CheckAdded probes clusters that were just added and reports each on one line.
func (a *App) CheckAdded(ctx context.Context, refs []store.Ref, timeout time.Duration) error {
	results, err := a.probeAll(ctx, refs, timeout)
	if err != nil {
		return err
	}
	failed := false
	for _, r := range results {
		parts := []string{"check " + r.Context, r.Status}
		if r.Health == "degraded" {
			parts = append(parts, "degraded: "+strings.Join(r.Failing, ", "))
		}
		for _, p := range []string{r.Version, probe.ShortUser(r.User), r.Error} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		fmt.Fprintln(a.Stdout, strings.Join(parts, "  "))
		failed = failed || r.Failed()
	}
	if failed {
		return ExitError(1)
	}
	return nil
}

// probeAll checks refs in parallel and remembers the results.
func (a *App) probeAll(ctx context.Context, refs []store.Ref, timeout time.Duration) ([]probe.Result, error) {
	results := make([]probe.Result, len(refs))
	sem := make(chan struct{}, probe.Parallel)
	var wg sync.WaitGroup
	for i, r := range refs {
		cfg, err := a.Store.Get(r)
		if err != nil {
			return nil, err
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			results[i] = probe.Probe(pctx, cfg, r.String())
		})
	}
	wg.Wait()
	if err := a.SaveChecks(results); err != nil {
		fmt.Fprintln(a.Stderr, "kx: save check results:", err)
	}
	return results, nil
}

// checkTargets picks explicitly named clusters (disabled ones included), all
// clusters with --all, or the enabled ones otherwise.
func (a *App) checkTargets(args []string, all bool) ([]store.Ref, error) {
	if len(args) > 0 {
		return a.Store.Expand(args)
	}
	refs, err := a.Store.Clusters()
	if err != nil || all {
		return refs, err
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return nil, err
	}
	var out []store.Ref
	for _, r := range refs {
		if st.Enabled(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (a *App) printCheck(results []probe.Result) error {
	if len(results) == 0 {
		fmt.Fprintln(a.Stderr, "nothing to check")
		return nil
	}
	known, err := a.LoadChecks()
	if err != nil {
		return err
	}
	policy := probe.NewVersionPolicy(known)
	o := table.New(a.Stdout)
	now := time.Now()
	cols := []table.Column{
		{Title: "CLUSTER", Shrink: 12},
		{Title: "STATUS"},
		{Title: "HEALTH"},
		{Title: "VERSION", Drop: 4},
		{Title: "NODES", Drop: 3},
		{Title: "USER", Shrink: 10, Trim: true, Drop: 2},
		{Title: "EXPIRES"},
		{Title: "LATENCY", Drop: 1},
	}
	rows := make([]table.Row, 0, len(results))
	for _, r := range results {
		status := table.Cell{Text: r.Status, Style: o.OK}
		if r.Status != "ok" {
			status.Style = o.Bad
		}
		expires := table.Cell{Text: probe.FormatExpiry(r.Expires, now), Style: o.Plain}
		switch {
		case expires.Text == "EXPIRED":
			expires.Style = o.Bad
		case strings.HasSuffix(expires.Text, "!"):
			expires.Style = o.Warn
		}
		latency := table.Cell{Text: fmt.Sprintf("%dms", r.Latency), Style: o.Dim}
		if r.Latency >= 1000 {
			latency.Style = o.Warn
		}
		rows = append(rows, table.Row{Cells: []table.Cell{
			{Text: r.Context, Style: o.Plain},
			status,
			HealthCell(o, r),
			VersionCell(o, r.Version, policy),
			NodesCell(o, r.Nodes),
			{Text: probe.ShortUser(r.User), Style: o.Plain},
			expires,
			latency,
		}})
	}
	if err := o.Table(cols, rows); err != nil {
		return err
	}
	// Problems go below the table: they are long and would break its layout.
	first := true
	for _, r := range results {
		for _, p := range r.Problems(policy) {
			if first {
				fmt.Fprintln(o.W)
				first = false
			}
			fmt.Fprintln(o.W, o.Paint(o.Bad, r.Context))
			for _, l := range strings.Split(o.WrapIndented(p, "  "), "\n") {
				fmt.Fprintln(o.W, o.Paint(o.Dim, l))
			}
		}
	}
	return nil
}

func HealthCell(o *table.Output, r probe.Result) table.Cell {
	switch r.Health {
	case "ok":
		return table.Cell{Text: "ok", Style: o.OK}
	case "degraded":
		return table.Cell{Text: "degraded", Style: o.Bad}
	}
	return table.Cell{}
}

func VersionCell(o *table.Output, v string, policy probe.VersionPolicy) table.Cell {
	if policy.Outdated(v) {
		return table.Cell{Text: v + "!", Style: o.Warn}
	}
	return table.Cell{Text: v, Style: o.Plain}
}

func NodesCell(o *table.Output, n *probe.NodeCount) table.Cell {
	if n == nil {
		return table.Cell{}
	}
	if n.Ready < n.Total {
		return table.Cell{Text: n.String(), Style: o.Warn}
	}
	return table.Cell{Text: n.String(), Style: o.Plain}
}

func (a *App) checksPath() string { return filepath.Join(a.Store.Dir(), "checks.json") }

// LoadChecks returns the last known result per context.
func (a *App) LoadChecks() (map[string]probe.Result, error) {
	return probe.LoadCache(a.checksPath())
}

// SaveChecks merges fresh results into the stored ones, forgetting clusters
// that are gone.
func (a *App) SaveChecks(fresh []probe.Result) error {
	checks, err := a.LoadChecks()
	if err != nil {
		checks = map[string]probe.Result{}
	}
	for _, r := range fresh {
		checks[r.Context] = r
	}
	refs, err := a.Store.Clusters()
	if err != nil {
		return err
	}
	for ctx := range checks {
		if !slices.ContainsFunc(refs, func(r store.Ref) bool { return r.String() == ctx }) {
			delete(checks, ctx)
		}
	}
	data, err := json.MarshalIndent(checks, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFile(a.checksPath(), data)
}

// CheckCell sums a result up in a word or two, worst problem first.
func CheckCell(o *table.Output, res probe.Result) table.Cell {
	exp := probe.FormatExpiry(res.Expires, time.Now())
	switch {
	case res.Status != "ok":
		return table.Cell{Text: res.Status, Style: o.Bad}
	case res.Health == "degraded":
		return table.Cell{Text: "degraded", Style: o.Bad}
	case exp == "EXPIRED":
		return table.Cell{Text: "ok, expired", Style: o.Bad}
	case res.Nodes != nil && res.Nodes.Ready < res.Nodes.Total:
		return table.Cell{Text: "ok, nodes " + res.Nodes.String(), Style: o.Warn}
	case strings.HasSuffix(exp, "!"):
		return table.Cell{Text: "ok, " + exp, Style: o.Warn}
	}
	return table.Cell{Text: "ok", Style: o.OK}
}
