package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// supportedMinors is how many minor releases upstream Kubernetes maintains.
const supportedMinors = 3

type tableColumn struct {
	Name string `json:"name"`
}

type nodeCount struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
}

func (n *nodeCount) String() string {
	if n == nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", n.Ready, n.Total)
}

// readyz asks the API server about its own health. It is usually open to
// anyone; a closed or missing endpoint leaves health unknown ("").
func readyz(ctx context.Context, hc *http.Client, base string) (health string, failing []string) {
	code, body, err := fetch(ctx, hc, http.MethodGet, base+"/readyz?verbose", nil, "text/plain", 1<<20)
	if err == nil && code == http.StatusNotFound {
		// Before 1.16 there is only /healthz.
		code, body, err = fetch(ctx, hc, http.MethodGet, base+"/healthz?verbose", nil, "text/plain", 1<<20)
	}
	switch {
	case err != nil, code == http.StatusUnauthorized, code == http.StatusForbidden, code == http.StatusNotFound:
		return "", nil
	case code == http.StatusOK:
		return "ok", nil
	}
	for _, l := range strings.Split(string(body), "\n") {
		if check, ok := strings.CutPrefix(l, "[-]"); ok {
			check, _, _ = strings.Cut(check, " ")
			failing = append(failing, check)
		}
	}
	return "degraded", failing
}

// countNodes counts Ready nodes. It asks for the compact Table rendering,
// a few hundred bytes per node; without permission to list nodes it gives nil.
func countNodes(ctx context.Context, hc *http.Client, base string) *nodeCount {
	const accept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json"
	code, body, err := fetch(ctx, hc, http.MethodGet, base+"/api/v1/nodes?includeObject=None", nil, accept, 32<<20)
	if err != nil || code != http.StatusOK {
		return nil
	}
	var resp struct {
		Kind    string        `json:"kind"`
		Columns []tableColumn `json:"columnDefinitions"`
		Rows    []struct {
			Cells []any `json:"cells"`
		} `json:"rows"`
		// A server that ignores the Table request sends a plain NodeList.
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return nil
	}
	if resp.Kind == "NodeList" {
		n := &nodeCount{Total: len(resp.Items)}
		for _, it := range resp.Items {
			for _, c := range it.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					n.Ready++
				}
			}
		}
		return n
	}
	col := slices.IndexFunc(resp.Columns, func(c tableColumn) bool { return c.Name == "Status" })
	if col < 0 {
		return nil
	}
	n := &nodeCount{Total: len(resp.Rows)}
	for _, r := range resp.Rows {
		if col >= len(r.Cells) {
			continue
		}
		// "Ready", "NotReady", "Ready,SchedulingDisabled", ...
		if s, _ := r.Cells[col].(string); slices.Contains(strings.Split(s, ","), "Ready") {
			n.Ready++
		}
	}
	return n
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

// minorVersion parses "v1.31.2-eks-1a2b" into 1, 31.
func minorVersion(v string) (major, minor int, ok bool) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}

// versionPolicy flags versions that fell out of upstream support. With no
// network lookup, the newest version among the user's own clusters stands in
// for the current release.
type versionPolicy struct {
	major, minor int
}

func newVersionPolicy(results map[string]checkResult) versionPolicy {
	var p versionPolicy
	for _, r := range results {
		if ma, mi, ok := minorVersion(r.Version); ok && (ma > p.major || ma == p.major && mi > p.minor) {
			p.major, p.minor = ma, mi
		}
	}
	return p
}

func (p versionPolicy) outdated(v string) bool {
	ma, mi, ok := minorVersion(v)
	return ok && ma == p.major && mi <= p.minor-supportedMinors
}

func (p versionPolicy) explain(v string) string {
	ma, mi, _ := minorVersion(v)
	return fmt.Sprintf("v%d.%d is out of upstream support (newest seen: v%d.%d)", ma, mi, p.major, p.minor)
}

func (r checkResult) failed() bool {
	return r.Status != "ok" || r.Health == "degraded"
}

func (s *store) checksPath() string { return filepath.Join(s.dir, "checks.json") }

// loadChecks returns the last known result per context.
func (s *store) loadChecks() (map[string]checkResult, error) {
	checks := map[string]checkResult{}
	data, err := os.ReadFile(s.checksPath())
	if errors.Is(err, fs.ErrNotExist) {
		return checks, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &checks); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.checksPath(), err)
	}
	return checks, nil
}

// saveChecks merges fresh results into the stored ones, forgetting clusters
// that are gone.
func (s *store) saveChecks(fresh []checkResult) error {
	checks, err := s.loadChecks()
	if err != nil {
		checks = map[string]checkResult{}
	}
	for _, r := range fresh {
		checks[r.Context] = r
	}
	refs, err := s.clusters()
	if err != nil {
		return err
	}
	for ctx := range checks {
		if !slices.ContainsFunc(refs, func(r ref) bool { return r.String() == ctx }) {
			delete(checks, ctx)
		}
	}
	data, err := json.MarshalIndent(checks, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(s.checksPath(), data)
}

func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
