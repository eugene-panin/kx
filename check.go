package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

const (
	checkParallel = 16
	expirySoon    = 30 * 24 * time.Hour
)

type checkResult struct {
	Context   string     `json:"context"`
	Status    string     `json:"status"`           // ok, unreachable, unauthorized, tls, error
	Health    string     `json:"health,omitempty"` // ok, degraded; empty when /readyz is closed
	Failing   []string   `json:"failing,omitempty"`
	Version   string     `json:"version,omitempty"`
	Nodes     *nodeCount `json:"nodes,omitempty"`
	User      string     `json:"user,omitempty"`
	Expires   *time.Time `json:"credentialsExpire,omitempty"`
	Latency   int64      `json:"latencyMs"`
	Error     string     `json:"error,omitempty"`
	CheckedAt time.Time  `json:"checkedAt"`
}

func (a *app) check(ctx context.Context, args []string, all bool, timeout time.Duration, asJSON bool) error {
	refs, err := a.checkTargets(args, all)
	if err != nil {
		return err
	}
	results := make([]checkResult, len(refs))
	sem := make(chan struct{}, checkParallel)
	var wg sync.WaitGroup
	for i, r := range refs {
		cfg, err := a.store.get(r)
		if err != nil {
			return err
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			results[i] = probe(pctx, cfg, r.String())
		})
	}
	wg.Wait()
	if err := a.store.saveChecks(results); err != nil {
		fmt.Fprintln(a.stderr, "kx: save check results:", err)
	}

	if asJSON {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else if err := a.printCheck(results); err != nil {
		return err
	}
	for _, res := range results {
		if res.failed() {
			return exitError(1)
		}
	}
	return nil
}

// checkTargets picks explicitly named clusters (disabled ones included), all
// clusters with --all, or the enabled ones otherwise.
func (a *app) checkTargets(args []string, all bool) ([]ref, error) {
	if len(args) > 0 {
		return a.store.expand(args)
	}
	refs, err := a.store.clusters()
	if err != nil || all {
		return refs, err
	}
	st, err := a.store.loadState()
	if err != nil {
		return nil, err
	}
	var out []ref
	for _, r := range refs {
		if st.enabled(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (a *app) printCheck(results []checkResult) error {
	if len(results) == 0 {
		fmt.Fprintln(a.stderr, "nothing to check")
		return nil
	}
	known, err := a.store.loadChecks()
	if err != nil {
		return err
	}
	policy := newVersionPolicy(known)
	o := newOutput(a.stdout)
	now := time.Now()
	cols := []column{
		{title: "CLUSTER", shrink: 12},
		{title: "STATUS"},
		{title: "HEALTH"},
		{title: "VERSION", drop: 4},
		{title: "NODES", drop: 3},
		{title: "USER", shrink: 10, trim: true, drop: 2},
		{title: "EXPIRES"},
		{title: "LATENCY", drop: 1},
	}
	rows := make([]row, 0, len(results))
	for _, r := range results {
		status := cell{r.Status, o.ok}
		if r.Status != "ok" {
			status.style = o.bad
		}
		expires := cell{formatExpiry(r.Expires, now), o.plain}
		switch {
		case expires.text == "EXPIRED":
			expires.style = o.bad
		case strings.HasSuffix(expires.text, "!"):
			expires.style = o.warn
		}
		latency := cell{fmt.Sprintf("%dms", r.Latency), o.dim}
		if r.Latency >= 1000 {
			latency.style = o.warn
		}
		rows = append(rows, row{cells: []cell{
			{r.Context, o.plain}, status, o.healthCell(r), o.versionCell(r.Version, policy),
			o.nodesCell(r.Nodes), {shortUser(r.User), o.plain}, expires, latency,
		}})
	}
	if err := o.table(cols, rows); err != nil {
		return err
	}
	// Problems go below the table: they are long and would break its layout.
	first := true
	for _, r := range results {
		for _, p := range r.problems(policy) {
			if first {
				fmt.Fprintln(o.w)
				first = false
			}
			fmt.Fprintln(o.w, o.paint(o.bad, r.Context))
			for _, l := range strings.Split(o.wrapIndented(p, "  "), "\n") {
				fmt.Fprintln(o.w, o.paint(o.dim, l))
			}
		}
	}
	return nil
}

// problems lists what is worth a line of its own under the table.
func (r checkResult) problems(policy versionPolicy) []string {
	var out []string
	if r.Error != "" {
		out = append(out, r.Error)
	}
	if r.Health == "degraded" {
		out = append(out, "readyz failing: "+strings.Join(r.Failing, ", "))
	}
	if policy.outdated(r.Version) {
		out = append(out, policy.explain(r.Version))
	}
	return out
}

func (o *output) healthCell(r checkResult) cell {
	switch r.Health {
	case "ok":
		return cell{"ok", o.ok}
	case "degraded":
		return cell{"degraded", o.bad}
	}
	return cell{}
}

func (o *output) versionCell(v string, policy versionPolicy) cell {
	if policy.outdated(v) {
		return cell{v + "!", o.warn}
	}
	return cell{v, o.plain}
}

func (o *output) nodesCell(n *nodeCount) cell {
	if n == nil {
		return cell{}
	}
	if n.Ready < n.Total {
		return cell{n.String(), o.warn}
	}
	return cell{n.String(), o.plain}
}

// shortUser abbreviates service account names, the usual long ones.
func shortUser(u string) string {
	if rest, ok := strings.CutPrefix(u, "system:serviceaccount:"); ok {
		if ns, name, ok := strings.Cut(rest, ":"); ok {
			return "sa:" + ns + "/" + name
		}
	}
	return u
}

// formatExpiry shows days left; the full date is in --json.
func formatExpiry(t *time.Time, now time.Time) string {
	if t == nil {
		return ""
	}
	left := t.Sub(now)
	days := int(left.Hours() / 24)
	switch {
	case left <= 0:
		return "EXPIRED"
	case left < expirySoon:
		return fmt.Sprintf("%dd!", days)
	default:
		return fmt.Sprintf("%dd", days)
	}
}

// probe asks the API server for its version and for who we are. /version is
// usually open to anonymous users, so only the second call proves the
// credentials work.
func probe(ctx context.Context, cfg *api.Config, name string) checkResult {
	res := checkResult{Context: name, Expires: credentialsExpiry(cfg, name), CheckedAt: time.Now()}
	var start time.Time
	fail := func(err error) checkResult {
		if !start.IsZero() {
			res.Latency = time.Since(start).Milliseconds()
		}
		res.Status, res.Error = classify(err)
		if res.Status == "unauthorized" && res.Expires != nil && res.Expires.Before(time.Now()) {
			res.Error = "credentials expired"
		}
		return res
	}

	rc, err := clientcmd.NewDefaultClientConfig(*cfg, &clientcmd.ConfigOverrides{CurrentContext: name}).ClientConfig()
	if err != nil {
		return fail(err)
	}
	if rc.ExecProvider != nil {
		// Probes run in parallel; a plugin prompting on stdin would hang them.
		rc.ExecProvider.StdinUnavailable = true
		rc.ExecProvider.StdinUnavailableMessage = "kx check runs non-interactively"
	}
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		return fail(err)
	}
	base := strings.TrimSuffix(rc.Host, "/")
	start = time.Now()

	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := call(ctx, hc, http.MethodGet, base+"/version", nil, &version); err != nil {
		return fail(err)
	}
	res.Version = version.GitVersion

	var review struct {
		Status struct {
			UserInfo struct {
				Username string `json:"username"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	body := []byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`)
	err = call(ctx, hc, http.MethodPost, base+"/apis/authentication.k8s.io/v1/selfsubjectreviews", body, &review)
	var he *httpError
	if errors.As(err, &he) && he.code == http.StatusNotFound {
		// Before 1.28 there is no SelfSubjectReview; discovery still requires
		// an authenticated user.
		err = call(ctx, hc, http.MethodGet, base+"/api", nil, nil)
	}
	if err != nil {
		return fail(err)
	}
	res.Latency = time.Since(start).Milliseconds()
	res.User = review.Status.UserInfo.Username
	res.Status = "ok"
	res.Health, res.Failing = readyz(ctx, hc, base)
	res.Nodes = countNodes(ctx, hc, base)
	return res
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("HTTP %d", e.code)
	}
	return fmt.Sprintf("HTTP %d: %s", e.code, e.msg)
}

func call(ctx context.Context, hc *http.Client, method, u string, body []byte, out any) error {
	code, data, err := fetch(ctx, hc, method, u, body, "application/json", 1<<20)
	if err != nil {
		return err
	}
	if code < 200 || code > 299 {
		var status struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &status)
		return &httpError{code: code, msg: status.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func fetch(ctx context.Context, hc *http.Client, method, u string, body []byte, accept string, limit int64) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return resp.StatusCode, data, err
}

func classify(err error) (status, detail string) {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var (
		he       *httpError
		ne       net.Error
		unknown  x509.UnknownAuthorityError
		invalid  x509.CertificateInvalidError
		hostname x509.HostnameError
		verify   *tls.CertificateVerificationError
	)
	switch {
	case errors.As(err, &he) && (he.code == http.StatusUnauthorized || he.code == http.StatusForbidden):
		// 403 on a whoami/discovery call means we came in as system:anonymous.
		return "unauthorized", err.Error()
	case errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &hostname), errors.As(err, &verify):
		return "tls", err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "unreachable", "timeout"
	case errors.As(err, &ne):
		return "unreachable", err.Error()
	default:
		return "error", err.Error()
	}
}

// credentialsExpiry reads the expiry of a client certificate or a JWT token.
// Exec plugins and opaque tokens yield nil.
func credentialsExpiry(cfg *api.Config, name string) *time.Time {
	ctx := cfg.Contexts[name]
	if ctx == nil {
		return nil
	}
	ai := cfg.AuthInfos[ctx.AuthInfo]
	if ai == nil {
		return nil
	}
	if block, _ := pem.Decode(ai.ClientCertificateData); block != nil {
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			return &cert.NotAfter
		}
	}
	return jwtExpiry(ai.Token)
}

func jwtExpiry(token string) *time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return nil
	}
	t := time.Unix(claims.Exp, 0)
	return &t
}
