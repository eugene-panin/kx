package probe

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
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

const (
	// Parallel caps concurrent probes.
	Parallel   = 16
	ExpirySoon = 30 * 24 * time.Hour
)

type Result struct {
	Context   string     `json:"context"`
	Status    string     `json:"status"`           // ok, unreachable, unauthorized, tls, error
	Health    string     `json:"health,omitempty"` // ok, degraded; empty when /readyz is closed
	Failing   []string   `json:"failing,omitempty"`
	Version   string     `json:"version,omitempty"`
	Nodes     *NodeCount `json:"nodes,omitempty"`
	User      string     `json:"user,omitempty"`
	Expires   *time.Time `json:"credentialsExpire,omitempty"`
	Latency   int64      `json:"latencyMs"`
	Error     string     `json:"error,omitempty"`
	CheckedAt time.Time  `json:"checkedAt"`
}

// Probe asks the API server for its version and for who we are. /version is
// usually open to anonymous users, so only the second call proves the
// credentials work.
func Probe(ctx context.Context, cfg *api.Config, name string) Result {
	res := Result{Context: name, Expires: credentialsExpiry(cfg, name), CheckedAt: time.Now()}
	var start time.Time
	fail := func(err error) Result {
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

// ShortUser abbreviates service account names, the usual long ones.
func ShortUser(u string) string {
	if rest, ok := strings.CutPrefix(u, "system:serviceaccount:"); ok {
		if ns, name, ok := strings.Cut(rest, ":"); ok {
			return "sa:" + ns + "/" + name
		}
	}
	return u
}

// FormatExpiry shows days left; the full date is in --json.
func FormatExpiry(t *time.Time, now time.Time) string {
	if t == nil {
		return ""
	}
	left := t.Sub(now)
	days := int(left.Hours() / 24)
	switch {
	case left <= 0:
		return "EXPIRED"
	case left < ExpirySoon:
		return fmt.Sprintf("%dd!", days)
	default:
		return fmt.Sprintf("%dd", days)
	}
}

// supportedMinors is how many minor releases upstream Kubernetes maintains.
const supportedMinors = 3

type tableColumn struct {
	Name string `json:"name"`
}

type NodeCount struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
}

func (n *NodeCount) String() string {
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
func countNodes(ctx context.Context, hc *http.Client, base string) *NodeCount {
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
		n := &NodeCount{Total: len(resp.Items)}
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
	n := &NodeCount{Total: len(resp.Rows)}
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

// VersionPolicy flags versions that fell out of upstream support. With no
// network lookup, the newest version among the user's own clusters stands in
// for the current release.
type VersionPolicy struct {
	major, minor int
}

func NewVersionPolicy(results map[string]Result) VersionPolicy {
	var p VersionPolicy
	for _, r := range results {
		if ma, mi, ok := minorVersion(r.Version); ok && (ma > p.major || ma == p.major && mi > p.minor) {
			p.major, p.minor = ma, mi
		}
	}
	return p
}

func (p VersionPolicy) Outdated(v string) bool {
	ma, mi, ok := minorVersion(v)
	return ok && ma == p.major && mi <= p.minor-supportedMinors
}

func (p VersionPolicy) Explain(v string) string {
	ma, mi, _ := minorVersion(v)
	return fmt.Sprintf("v%d.%d is out of upstream support (newest seen: v%d.%d)", ma, mi, p.major, p.minor)
}

func (r Result) Failed() bool {
	return r.Status != "ok" || r.Health == "degraded"
}

// Problems lists what is worth a line of its own under the table.
func (r Result) Problems(policy VersionPolicy) []string {
	var out []string
	if r.Error != "" {
		out = append(out, r.Error)
	}
	if r.Health == "degraded" {
		out = append(out, "readyz failing: "+strings.Join(r.Failing, ", "))
	}
	if policy.Outdated(r.Version) {
		out = append(out, policy.Explain(r.Version))
	}
	return out
}

func FormatAge(d time.Duration) string {
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

// LoadCache reads saved results, keyed by context.
func LoadCache(path string) (map[string]Result, error) {
	checks := map[string]Result{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return checks, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &checks); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return checks, nil
}
