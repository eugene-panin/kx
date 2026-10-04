package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eugene-panin/kx/internal/app"
	"github.com/eugene-panin/kx/internal/kxtest"
	"github.com/eugene-panin/kx/internal/probe"
	"github.com/eugene-panin/kx/internal/store"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// kxtest.Kubeadm-style kubeconfig: every cluster built this way has the same
// env runs the real CLI against an isolated kxtest.Env.
type env struct {
	*kxtest.Env
	t *testing.T
}

func newEnv(t *testing.T) *env { return &env{kxtest.NewEnv(t), t} }

func (e *env) run(stdin string, args ...string) (string, error) {
	e.t.Helper()
	var out, errOut bytes.Buffer
	err := run(args, strings.NewReader(stdin), &out, &errOut)
	return out.String() + errOut.String(), err
}

func (e *env) ok(args ...string) string {
	e.t.Helper()
	out, err := e.run("", args...)
	if err != nil {
		e.t.Fatalf("kx %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func TestAddResolvesNameClashes(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a:6443"), "-c", "acme", "--name", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b:6443"), "-c", "acme", "--name", "stage")
	e.ok("add", e.Kubeconfig("c.yaml", "https://c:6443"), "-c", "globex")

	e.WantContexts("acme/prod", "acme/stage", "globex/kubernetes-admin-kubernetes")

	cfg, err := clientcmd.LoadFromFile(e.Target)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"acme/prod": "https://a:6443", "acme/stage": "https://b:6443"} {
		ctx := cfg.Contexts[name]
		cl := cfg.Clusters[ctx.Cluster]
		if cl.Server != want {
			t.Errorf("%s server = %s, want %s", name, cl.Server, want)
		}
		if string(cl.CertificateAuthorityData) != "CA" || cl.CertificateAuthority != "" {
			t.Errorf("%s: CA not inlined: %+v", name, cl)
		}
		if cfg.AuthInfos[ctx.AuthInfo].Token != "secret" {
			t.Errorf("%s: user not carried over", name)
		}
	}

	if _, err := e.run("", "add", e.Kubeconfig("d.yaml", "https://d:6443"), "-c", "acme", "--name", "prod"); err == nil {
		t.Error("adding an existing cluster without --force succeeded")
	}
	e.ok("add", e.Src+"/d.yaml", "-c", "acme", "--name", "prod", "--force")
	cfg, _ = clientcmd.LoadFromFile(e.Target)
	if s := cfg.Clusters["acme/prod"].Server; s != "https://d:6443" {
		t.Errorf("after --force server = %s", s)
	}
}

func TestAddFromStdin(t *testing.T) {
	e := newEnv(t)
	t.Chdir(e.Src)
	cfg := strings.Replace(kxtest.Kubeadm, "%s", "https://x:6443", 1)
	if out, err := e.run(cfg, "add", "-", "-c", "acme", "-n", "prod"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	e.WantContexts("acme/prod")
}

func TestOnOff(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("add", e.Kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")

	e.ok("off", "acme")
	e.WantContexts("globex/main")

	e.ok("on", "acme/stage")
	e.WantContexts("acme/stage", "globex/main")

	e.ok("off", "globex/main")
	e.WantContexts("acme/stage")

	e.ok("on", "acme", "globex")
	e.WantContexts("acme/prod", "acme/stage", "globex/main")

	if out := e.ok("ls"); !strings.Contains(out, "acme") || strings.Contains(out, "off") {
		t.Errorf("ls after enabling everything:\n%s", out)
	}
	if _, err := e.run("", "off", "nope"); err == nil {
		t.Error("off on unknown client succeeded")
	}
}

func TestMove(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "unsorted")
	e.ok("off", "unsorted")

	e.ok("mv", "unsorted/kubernetes-admin-kubernetes", "acme")
	e.ok("mv", "acme/kubernetes-admin-kubernetes", "acme/prod")
	e.ok("on", "acme")
	e.WantContexts("acme/prod")

	cfg, err := clientcmd.LoadFromFile(e.Target)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CurrentContext = "acme/prod"
	if err := clientcmd.WriteToFile(*cfg, e.Target); err != nil {
		t.Fatal(err)
	}
	e.ok("mv", "acme", "acme-corp")
	e.WantContexts("acme-corp/prod")
	if cfg, _ = clientcmd.LoadFromFile(e.Target); cfg.CurrentContext != "acme-corp/prod" {
		t.Errorf("current-context = %q after renaming it", cfg.CurrentContext)
	}

	cfg, err = clientcmd.LoadFromFile(filepath.Join(e.Home, "clusters", "acme-corp", "prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := cfg.Contexts["acme-corp/prod"]
	if ctx == nil || ctx.Cluster != "acme-corp/prod" || ctx.AuthInfo != "acme-corp/prod" || cfg.CurrentContext != "acme-corp/prod" {
		t.Errorf("names not rewritten: %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(e.Home, "clusters", "unsorted")); !os.IsNotExist(err) {
		t.Error("empty client directory left behind")
	}
}

func TestMovedDisabledClusterStaysOff(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "unsorted", "-n", "x")
	e.ok("off", "unsorted")
	e.ok("mv", "unsorted/x", "acme")
	e.WantContexts()
	if out := e.ok("ls"); !strings.Contains(out, "off") {
		t.Errorf("moved cluster lost its off state:\n%s", out)
	}
}

func TestRemove(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("add", e.Kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")
	e.ok("off", "acme")

	// Without a terminal kx never asks: a piped answer is not taken, the
	// question isn't printed, nothing is removed, and the exit code is 2.
	for _, args := range [][]string{{"rm", "acme"}, {"rm", "globex/main"}, {"rm", "globex"}} {
		for _, stdin := range []string{"", "y\n"} {
			out, err := e.run(stdin, args...)
			var usage *app.UsageError
			if !errors.As(err, &usage) || report(err, io.Discard) != 2 {
				t.Errorf("kx %v with stdin %q: err = %v, want a usage error", args, stdin, err)
			}
			if strings.Contains(out, "[y/N]") {
				t.Errorf("kx %v asked without a terminal:\n%s", args, out)
			}
		}
	}
	if out := e.ok("ls"); !strings.Contains(out, "stage") || !strings.Contains(out, "main") {
		t.Fatalf("refused rm removed clusters:\n%s", out)
	}

	e.ok("rm", "acme", "-y")
	e.WantContexts("globex/main")

	// A client re-added under the same name must not inherit the old off state.
	e.ok("add", e.Src+"/a.yaml", "-c", "acme", "-n", "prod")
	e.WantContexts("acme/prod", "globex/main")
}

func TestForeignContextsAreProtected(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")

	// Something like `aws eks update-kubeconfig` writes straight into the target.
	cfg, err := clientcmd.LoadFromFile(e.Target)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := clientcmd.LoadFromFile(e.Kubeconfig("eks.yaml", "https://eks"))
	if err != nil {
		t.Fatal(err)
	}
	eks := "arn:aws:eks:eu-west-1:123:cluster/shop"
	cfg.Clusters[eks] = foreign.Clusters["kubernetes"]
	cfg.Clusters[eks].CertificateAuthority, cfg.Clusters[eks].CertificateAuthorityData = "", []byte("CA")
	cfg.AuthInfos[eks] = foreign.AuthInfos["kubernetes-admin"]
	ctx := foreign.Contexts["kubernetes-admin@kubernetes"]
	ctx.Cluster, ctx.AuthInfo = eks, eks
	cfg.Contexts[eks] = ctx
	cfg.CurrentContext = eks
	if err := clientcmd.WriteToFile(*cfg, e.Target); err != nil {
		t.Fatal(err)
	}

	out, err := e.run("", "off", "acme")
	if err == nil || !strings.Contains(err.Error(), eks) {
		t.Fatalf("off with a foreign context: err = %v\n%s", err, out)
	}
	e.WantContexts("acme/prod", eks)

	e.ok("import-current", "-c", "globex")
	e.WantContexts("acme/prod", "globex/shop")
	if cfg, _ = clientcmd.LoadFromFile(e.Target); cfg.CurrentContext != "globex/shop" {
		t.Errorf("current-context = %q after import", cfg.CurrentContext)
	}
	e.ok("off", "acme")
	e.WantContexts("globex/shop")

	backups, err := os.ReadDir(filepath.Join(e.Home, "backups"))
	if err != nil || len(backups) == 0 {
		t.Errorf("no backups written: %v", err)
	}
}

func TestBuildKeepsCurrentContext(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")

	cfg, _ := clientcmd.LoadFromFile(e.Target)
	cfg.CurrentContext = "acme/stage"
	if err := clientcmd.WriteToFile(*cfg, e.Target); err != nil {
		t.Fatal(err)
	}
	e.ok("off", "acme/prod")
	if cfg, _ = clientcmd.LoadFromFile(e.Target); cfg.CurrentContext != "acme/stage" {
		t.Errorf("current-context = %q, want acme/stage", cfg.CurrentContext)
	}
	e.ok("off", "acme/stage")
	if cfg, _ = clientcmd.LoadFromFile(e.Target); cfg.CurrentContext != "" {
		t.Errorf("current-context = %q after disabling it", cfg.CurrentContext)
	}
}

func TestNamespaceSwitchSurvivesRebuild(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "globex", "-n", "main")

	// What kubens / k9s do when switching namespace.
	cfg, _ := clientcmd.LoadFromFile(e.Target)
	cfg.Contexts["acme/prod"].Namespace = "monitoring"
	if err := clientcmd.WriteToFile(*cfg, e.Target); err != nil {
		t.Fatal(err)
	}

	if out := e.ok("ls", "acme"); !strings.Contains(out, "monitoring") {
		t.Errorf("ls before any rebuild:\n%s", out)
	}
	e.ok("off", "globex")
	e.ok("off", "acme")
	e.ok("on", "acme")
	if cfg, _ = clientcmd.LoadFromFile(e.Target); cfg.Contexts["acme/prod"].Namespace != "monitoring" {
		t.Errorf("namespace = %q, want monitoring", cfg.Contexts["acme/prod"].Namespace)
	}
	if out := e.ok("ls"); !strings.Contains(out, "monitoring") {
		t.Errorf("ls does not show the switched namespace:\n%s", out)
	}
}

func TestExport(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("off", "acme/stage")

	cfg, err := clientcmd.Load([]byte(e.ok("export", "acme")))
	if err != nil {
		t.Fatal(err)
	}
	if got := store.ContextNames(cfg); !slices.Equal(got, []string{"acme/prod", "acme/stage"}) {
		t.Errorf("exported contexts = %v", got)
	}
}

func TestListJSON(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "globex", "-n", "main")
	e.ok("off", "globex")

	var rows []app.ListRow
	out := e.ok("ls", "--json")
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []app.ListRow{
		{Client: "acme", Cluster: "prod", Context: "acme/prod", Server: "https://a", Enabled: true},
		{Client: "globex", Cluster: "main", Context: "globex/main", Server: "https://b"},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("rows = %+v\nwant %+v", rows, want)
	}
	if strings.Contains(out, "secret") {
		t.Error("ls --json leaks credentials")
	}

	empty := newEnv(t)
	if out := strings.TrimSpace(empty.ok("ls", "--json")); out != "[]" {
		t.Errorf("empty ls --json = %q, want []", out)
	}
}

func TestExec(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("add", e.Kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")
	e.ok("off", "acme/stage")

	out := e.ok("exec", "acme", "--", "sh", "-c", `echo "$KX_SCOPE"; echo "$KUBECONFIG"; cat "$KUBECONFIG"`)
	scope, rest, _ := strings.Cut(out, "\n")
	path, data, _ := strings.Cut(rest, "\n")
	if scope != "acme" {
		t.Errorf("KX_SCOPE = %q", scope)
	}
	cfg, err := clientcmd.Load([]byte(data))
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if got := store.ContextNames(cfg); !slices.Equal(got, []string{"acme/prod", "acme/stage"}) {
		t.Errorf("exec sees contexts %v", got)
	}
	if cfg.CurrentContext != "acme/prod" {
		t.Errorf("current-context = %q", cfg.CurrentContext)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp kubeconfig %s left behind", path)
	}

	_, err = e.run("", "exec", "globex/main", "--", "sh", "-c", "exit 3")
	if code, ok := errors.AsType[app.ExitError](err); !ok || code != 3 {
		t.Errorf("exit status: err = %v", err)
	}
	for _, args := range [][]string{
		{"exec", "acme", "sh"},
		{"exec", "--", "sh"},
		{"exec", "acme", "--"},
		{"exec", "nope", "--", "true"},
	} {
		if _, err := e.run("", args...); err == nil {
			t.Errorf("kx %v succeeded", args)
		}
	}
}

func TestUse(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("off", "acme/stage")
	backups := func() int {
		entries, _ := os.ReadDir(filepath.Join(e.Home, "backups"))
		return len(entries)
	}
	before := backups()

	if out := e.ok("use"); strings.TrimSpace(out) != "no current context" {
		t.Errorf("use without current = %q", out)
	}
	e.ok("use", "acme/prod")
	if cfg, _ := clientcmd.LoadFromFile(e.Target); cfg.CurrentContext != "acme/prod" {
		t.Errorf("current-context = %q", cfg.CurrentContext)
	}
	if out := e.ok("use"); strings.TrimSpace(out) != "acme/prod" {
		t.Errorf("use prints %q", out)
	}
	if out := e.ok("ls"); !slices.ContainsFunc(strings.Split(out, "\n"), func(l string) bool {
		f := strings.Fields(l)
		return len(f) > 2 && f[0] == "*" && f[1] == "acme" && f[2] == "prod"
	}) {
		t.Errorf("ls does not mark the current cluster:\n%s", out)
	}
	if backups() != before {
		t.Error("use wrote a backup")
	}

	for _, bad := range []string{"acme", "acme/stage", "acme/nope"} {
		if _, err := e.run("", "use", bad); err == nil {
			t.Errorf("use %s succeeded", bad)
		}
	}

	// Turning the current cluster off clears current-context rather than
	// leaving kubectl pointed at something else.
	e.ok("off", "acme/prod")
	if cfg, _ := clientcmd.LoadFromFile(e.Target); cfg.CurrentContext != "" {
		t.Errorf("current-context = %q after disabling it", cfg.CurrentContext)
	}
}

// fakeAPI is a TLS API server that accepts the token from the kxtest.Kubeadm fixture.
func TestCheck(t *testing.T) {
	e := newEnv(t)
	ok := kxtest.FakeAPI(t, kxtest.APIOpts{})
	old := kxtest.FakeAPI(t, kxtest.APIOpts{NoSSR: true})
	slow := kxtest.FakeAPI(t, kxtest.APIOpts{Hang: true})
	gone := kxtest.FakeAPI(t, kxtest.APIOpts{})
	gone.Close()

	e.WriteCA(ok)
	e.ok("add", e.Kubeconfig("ok.yaml", ok.URL), "-c", "acme", "-n", "ok")
	e.ok("add", e.Kubeconfig("old.yaml", old.URL), "-c", "acme", "-n", "old")
	e.ok("add", e.Kubeconfig("slow.yaml", slow.URL), "-c", "acme", "-n", "slow")
	e.ok("add", e.Kubeconfig("gone.yaml", gone.URL), "-c", "acme", "-n", "gone")
	e.ok("add", e.Kubeconfig("bad.yaml", ok.URL), "-c", "acme", "-n", "badtoken")
	// A token changed by hand in ~/.kube/config is what check must use.
	cfg, err := clientcmd.LoadFromFile(e.Target)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthInfos["acme/badtoken"].Token = "wrong"
	if err := clientcmd.WriteToFile(*cfg, e.Target); err != nil {
		t.Fatal(err)
	}
	e.ok("off", "acme/old")

	out, err := e.run("", "check", "--json", "--timeout", "300ms")
	if _, isExit := err.(app.ExitError); !isExit {
		t.Fatalf("check: err = %v\n%s", err, out)
	}
	var results []probe.Result
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]probe.Result{}
	for _, r := range results {
		got[r.Context] = r
	}
	if _, checked := got["acme/old"]; checked {
		t.Error("disabled cluster checked without --all")
	}
	if r := got["acme/ok"]; r.Status != "ok" || r.Version != "v1.31.2" || r.User != "kubernetes-admin" {
		t.Errorf("ok cluster: %+v", r)
	}
	if r := got["acme/badtoken"]; r.Status != "unauthorized" {
		t.Errorf("bad token: %+v", r)
	}
	if r := got["acme/gone"]; r.Status != "unreachable" {
		t.Errorf("closed server: %+v", r)
	}
	if r := got["acme/slow"]; r.Status != "unreachable" || r.Error != "timeout" {
		t.Errorf("hanging server: %+v", r)
	}

	// Named explicitly, a disabled cluster is checked; without SelfSubjectReview
	// the discovery fallback still proves the credentials.
	out, err = e.run("", "check", "acme/old", "acme/ok")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	statuses := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && strings.HasPrefix(f[0], "acme/") {
			statuses[f[0]] = f[1]
		}
	}
	if len(statuses) != 2 || statuses["acme/old"] != "ok" || statuses["acme/ok"] != "ok" {
		t.Errorf("explicit check:\n%s", out)
	}
}

func TestCheckHealthNodesVersions(t *testing.T) {
	e := newEnv(t)
	fresh := kxtest.FakeAPI(t, kxtest.APIOpts{Version: "v1.36.2", Nodes: []string{"Ready", "Ready,SchedulingDisabled", "NotReady"}})
	stale := kxtest.FakeAPI(t, kxtest.APIOpts{Version: "v1.33.4-eks-1a2b", Failing: []string{"etcd"}})
	e.WriteCA(fresh)
	e.ok("add", e.Kubeconfig("a.yaml", fresh.URL), "-c", "acme", "-n", "fresh")
	e.ok("add", e.Kubeconfig("b.yaml", stale.URL), "-c", "acme", "-n", "stale")

	out, err := e.run("", "check", "--json")
	if _, isExit := err.(app.ExitError); !isExit {
		t.Fatalf("a degraded cluster must fail check: err = %v\n%s", err, out)
	}
	var results []probe.Result
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]probe.Result{}
	for _, r := range results {
		got[r.Context] = r
	}
	if r := got["acme/fresh"]; r.Health != "ok" || r.Nodes == nil || *r.Nodes != (probe.NodeCount{Ready: 2, Total: 3}) {
		t.Errorf("fresh: %+v nodes=%v", r, r.Nodes)
	}
	if r := got["acme/stale"]; r.Health != "degraded" || len(r.Failing) != 1 || r.Failing[0] != "etcd" || r.Nodes != nil {
		t.Errorf("stale: %+v", r)
	}

	out, _ = e.run("", "check")
	for _, want := range []string{"2/3", "degraded", "v1.33.4-eks-1a2b!", "readyz failing: etcd", "v1.33 is out of upstream support (newest seen: v1.36)"} {
		if !strings.Contains(out, want) {
			t.Errorf("check output lacks %q:\n%s", want, out)
		}
	}

	// Results are remembered for ls.
	var rows []app.ListRow
	if err := json.Unmarshal([]byte(e.ok("ls", "--json")), &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].Version != "v1.36.2" || rows[1].Version != "v1.33.4-eks-1a2b" {
		t.Errorf("ls versions: %+v", rows)
	}
	if out := e.ok("ls"); !strings.Contains(out, "v1.36.2") {
		t.Errorf("ls table lacks the version:\n%s", out)
	}
}

func TestNamespace(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("off", "acme/stage")
	nsOf := func(path, ctx string) string {
		t.Helper()
		cfg, err := clientcmd.LoadFromFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Contexts[ctx].Namespace
	}
	backups := func() int {
		entries, _ := os.ReadDir(filepath.Join(e.Home, "backups"))
		return len(entries)
	}
	before := backups()

	for _, args := range [][]string{{"ns"}, {"ns", "monitoring"}} {
		if _, err := e.run("", args...); err == nil {
			t.Errorf("kx %v without a current context succeeded", args)
		}
	}

	e.ok("use", "acme/prod")
	if out := strings.TrimSpace(e.ok("ns")); out != "default" {
		t.Errorf("ns with none set = %q, want default", out)
	}
	e.ok("ns", "monitoring")
	if got := nsOf(e.Target, "acme/prod"); got != "monitoring" {
		t.Errorf("target namespace = %q", got)
	}
	if got := nsOf(filepath.Join(e.Home, "clusters", "acme", "prod.yaml"), "acme/prod"); got != "monitoring" {
		t.Errorf("stored namespace = %q", got)
	}
	if out := strings.TrimSpace(e.ok("ns")); out != "monitoring" {
		t.Errorf("ns = %q", out)
	}
	if backups() != before {
		t.Error("ns wrote a backup")
	}

	// A cluster that is off only lives in the store; it keeps the namespace
	// and brings it back when turned on.
	e.ok("ns", "acme/stage", "web")
	e.ok("on", "acme/stage")
	if got := nsOf(e.Target, "acme/stage"); got != "web" {
		t.Errorf("namespace after turning on = %q", got)
	}

	for _, bad := range [][]string{{"ns", "Bad_NS"}, {"ns", "acme", "web"}, {"ns", "acme/nope", "web"}} {
		if _, err := e.run("", bad...); err == nil {
			t.Errorf("kx %v succeeded", bad)
		}
	}
}

func TestAddRejectsBrokenConfigs(t *testing.T) {
	e := newEnv(t)
	t.Chdir(e.Src) // the fixture's ca.crt is relative
	noCreds := strings.Replace(strings.Replace(kxtest.Kubeadm, "%s", "https://a", 1), "    token: secret\n", "    {}\n", 1)
	noCreds = strings.Replace(noCreds, "  user:\n    {}\n", "  user: {}\n", 1)
	out, err := e.run(noCreds, "add", "-", "-c", "acme")
	if err == nil || !strings.Contains(err.Error(), `context "kubernetes-admin@kubernetes": no credentials`) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	noServer := strings.Replace(kxtest.Kubeadm, "    server: %s\n", "", 1)
	if _, err := e.run(noServer, "add", "-", "-c", "acme"); err == nil || !strings.Contains(err.Error(), "no server address") {
		t.Errorf("no server: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Home, "clusters", "acme")); !os.IsNotExist(err) {
		t.Error("a rejected config left files in the store")
	}
}

func TestAddCheck(t *testing.T) {
	e := newEnv(t)
	srv := kxtest.FakeAPI(t, kxtest.APIOpts{Version: "v1.36.2"})
	e.WriteCA(srv)

	out := e.ok("add", e.Kubeconfig("a.yaml", srv.URL), "-c", "acme", "-n", "prod", "--check")
	if !strings.Contains(out, "check acme/prod  ok  v1.36.2  kubernetes-admin") {
		t.Errorf("add --check output:\n%s", out)
	}

	bad := strings.Replace(strings.Replace(kxtest.Kubeadm, "%s", srv.URL, 1), "token: secret", "token: wrong", 1)
	bad = strings.Replace(bad, "certificate-authority: ca.crt", "certificate-authority: "+filepath.Join(e.Src, "ca.crt"), 1)
	out, err := e.run(bad, "add", "-", "-c", "acme", "-n", "stale", "--check")
	if _, isExit := err.(app.ExitError); !isExit || !strings.Contains(out, "check acme/stale  unauthorized") {
		t.Errorf("failing check: err = %v\n%s", err, out)
	}
	// The cluster is added all the same, and the result is remembered.
	e.WantContexts("acme/prod", "acme/stale")
	checks, err := probe.LoadCache(filepath.Join(e.Home, "checks.json"))
	if err != nil || checks["acme/stale"].Status != "unauthorized" || checks["acme/prod"].Status != "ok" {
		t.Errorf("saved checks = %+v, %v", checks, err)
	}
}

// editTarget changes ~/.kube/config the way a person or another tool would.
func editTarget(t *testing.T, e *env, edit func(*api.Config)) {
	t.Helper()
	cfg, err := clientcmd.LoadFromFile(e.Target)
	if err != nil {
		t.Fatal(err)
	}
	edit(cfg)
	if err := clientcmd.WriteToFile(*cfg, e.Target); err != nil {
		t.Fatal(err)
	}
}

func TestSyncTakesHandEdits(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("add", e.Kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")

	editTarget(t, e, func(cfg *api.Config) {
		cfg.Clusters["acme/prod"].Server = "https://moved:6443"
		cfg.AuthInfos["acme/prod"].Token = "rotated"
		delete(cfg.Contexts, "acme/stage")
		cfg.Contexts["globex/main"].Namespace = "web"
	})

	out := e.ok("ls")
	for _, want := range []string{
		"acme/prod: took cluster and credentials from",
		"acme/stage: removed from",
		"by hand, turned off",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "globex/main: took") {
		t.Errorf("a namespace switch is reported:\n%s", out)
	}

	stored, err := clientcmd.LoadFromFile(filepath.Join(e.Home, "clusters", "acme", "prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Clusters["acme/prod"].Server != "https://moved:6443" || stored.AuthInfos["acme/prod"].Token != "rotated" {
		t.Errorf("store not updated: %+v %+v", stored.Clusters["acme/prod"], stored.AuthInfos["acme/prod"])
	}
	if ns := stored.Contexts["acme/prod"].Namespace; ns != "" {
		t.Errorf("unrelated namespace changed: %q", ns)
	}

	// The removed cluster is off, not gone, and the edits survive a rebuild.
	e.ok("on", "acme/stage")
	e.WantContexts("acme/prod", "acme/stage", "globex/main")
	cfg, _ := clientcmd.LoadFromFile(e.Target)
	if cfg.Clusters["acme/prod"].Server != "https://moved:6443" || cfg.Contexts["globex/main"].Namespace != "web" {
		t.Error("a rebuild undid the hand edits")
	}
	if out := strings.TrimSpace(e.ok("sync")); out != "nothing to sync" {
		t.Errorf("second sync = %q", out)
	}
}

func TestSyncAfterTargetIsGone(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	if err := os.Remove(e.Target); err != nil {
		t.Fatal(err)
	}
	if out := e.ok("ls"); strings.Contains(out, "turned off") {
		t.Errorf("a deleted file turned clusters off:\n%s", out)
	}
	e.ok("build")
	e.WantContexts("acme/prod")
}

func TestBrokenTargetDoesNotBlockReads(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	if err := os.WriteFile(e.Target, []byte("not: [yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := e.ok("ls")
	if !strings.Contains(out, "kx: sync with") || !strings.Contains(out, "prod") {
		t.Errorf("ls with a broken target:\n%s", out)
	}
}

func TestEmptiedTargetIsAReset(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	if err := os.WriteFile(e.Target, nil, 0o600); err != nil { // `: > ~/.kube/config`
		t.Fatal(err)
	}
	out := e.ok("ls")
	if strings.Contains(out, "turned off") || strings.Contains(out, " off") {
		t.Errorf("an emptied kubeconfig turned clusters off:\n%s", out)
	}
	if !strings.Contains(out, "kx build writes them again") {
		t.Errorf("no hint about the reset:\n%s", out)
	}
	e.ok("build")
	e.WantContexts("acme/prod", "acme/stage")
}

func TestToggleIsAllOrNothing(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	out, err := e.run("", "off", "acme/prod", "nope")
	if err == nil || strings.Contains(out, "acme/prod: off") {
		t.Errorf("off with a bad argument: err = %v\n%s", err, out)
	}
	e.WantContexts("acme/prod")
}

func TestAddRefusesSymlinkedTarget(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	link := filepath.Join(e.Src, "dotfiles-config")
	if err := os.Symlink(e.Target, link); err != nil {
		t.Fatal(err)
	}
	// Point kx at the link the way a dotfiles setup does, then feed it back.
	t.Setenv("KX_KUBECONFIG", link)
	if _, err := e.run("", "add", link, "-c", "dup"); err == nil {
		t.Error("adding the generated file through a symlink succeeded")
	}
	e.WantContexts("acme/prod")
}

func TestBuildSaysWhatItDid(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	editTarget(t, e, func(cfg *api.Config) {
		cfg.Contexts["eks"] = cfg.Contexts["acme/prod"].DeepCopy()
	})
	out := e.ok("build", "--force")
	for _, want := range []string{"dropped eks", "wrote ", "1 cluster\n", "previous version: "} {
		if !strings.Contains(out, want) {
			t.Errorf("build --force output lacks %q:\n%s", want, out)
		}
	}
}

func TestLosingCurrentContextIsReported(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("use", "acme/prod")
	if out := e.ok("off", "acme"); !strings.Contains(out, "current-context acme/prod is off or gone") {
		t.Errorf("turning off the current cluster says nothing about it:\n%s", out)
	}
}

func TestMoveIntoDisabledClientIsReported(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "stage")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "globex", "-n", "main")
	e.ok("off", "globex")
	if out := e.ok("mv", "acme/stage", "globex"); !strings.Contains(out, "globex/stage is off now") {
		t.Errorf("mv into a disabled client is silent about turning it off:\n%s", out)
	}
}

func TestUsageErrors(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"--bogus"}, {"add"}, {"lss"}, {"check", "--timeout", "0s"}, {"add", "-c"}} {
		out, err := e.run("", args...)
		if code := report(err, io.Discard); code != 2 {
			t.Errorf("kx %v: exit %d, want 2 (%v)\n%s", args, code, err, out)
		}
	}
	if _, err := e.run("", "lss"); err == nil || !strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("no suggestion for a typo: %v", err)
	}
	out, err := e.run("", "add", "--bogus", "-h")
	if err != nil || !strings.Contains(out, "Usage:") {
		t.Errorf("-h after a bad flag: err = %v\n%s", err, out)
	}
}

func TestDryRuns(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "--name", "prod")
	if out := e.ok("rm", "acme", "--dry-run"); !strings.Contains(out, "would remove acme/prod") {
		t.Errorf("rm --dry-run:\n%s", out)
	}
	editTarget(t, e, func(cfg *api.Config) { cfg.Contexts["eks"] = cfg.Contexts["acme/prod"].DeepCopy() })
	out := e.ok("build", "--force", "--dry-run")
	if !strings.Contains(out, "would drop eks") || !strings.Contains(out, "would write") {
		t.Errorf("build --force --dry-run:\n%s", out)
	}
	e.WantContexts("acme/prod", "eks")
}

func TestQuietAndNoInput(t *testing.T) {
	e := newEnv(t)
	if out := e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "--name", "prod", "-q"); strings.TrimSpace(out) != "" {
		t.Errorf("add -q printed %q", out)
	}
	_, err := e.run("", "rm", "acme", "--no-input")
	var usage *app.UsageError
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "--no-input") {
		t.Errorf("rm --no-input: %v", err)
	}
}

func TestNotAKubeconfig(t *testing.T) {
	e := newEnv(t)
	_, err := e.run("just some text\n", "add", "-", "-c", "acme")
	if err == nil || !strings.Contains(err.Error(), "stdin is not a kubeconfig") || strings.Contains(err.Error(), "struct") {
		t.Errorf("garbage on stdin: %v", err)
	}
}

func TestPipedTableHasNoEmptyCells(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "--name", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "--name", "stage")
	e.ok("use", "acme/prod")
	// The same fields on every line, header and current row included, so
	// awk '{print $2}' is the client everywhere.
	for _, l := range strings.Split(strings.TrimSpace(e.ok("ls")), "\n") {
		if f := strings.Fields(l); len(f) != 7 {
			t.Errorf("row has %d fields, empty cells must be '-': %q", len(f), l)
		}
	}
}

func TestDeprecatedShorthands(t *testing.T) {
	e := newEnv(t)
	out := e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	if !strings.Contains(out, "use --name") {
		t.Errorf("add -n gives no deprecation warning:\n%s", out)
	}
	e.WantContexts("acme/prod")
	if out := e.ok("-v"); !strings.Contains(out, "use --version") {
		t.Errorf("-v gives no deprecation warning:\n%s", out)
	}
}

func TestOldFilesMoveToXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KX_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("KX_KUBECONFIG", filepath.Join(home, "kubeconfig"))
	old := filepath.Join(home, ".config", "kx")
	if err := os.MkdirAll(filepath.Join(old, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(old, "backups", "config-1"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(old, "checks.json"), []byte("{}"), 0o600)

	var out, errOut bytes.Buffer
	if err := run([]string{"ls"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".local/state/kx/backups/config-1", ".cache/kx/checks.json"} {
		if _, err := os.Stat(filepath.Join(home, p)); err != nil {
			t.Errorf("%s not moved: %v", p, err)
		}
	}
	if !strings.Contains(errOut.String(), "moved backups") {
		t.Errorf("move not reported: %q", errOut.String())
	}

	// An older kx writing to the old place again gets merged in, not stranded.
	os.MkdirAll(filepath.Join(old, "backups"), 0o700)
	os.WriteFile(filepath.Join(old, "backups", "config-2"), []byte("y"), 0o600)
	if err := run([]string{"ls"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/state/kx/backups/config-2")); err != nil {
		t.Errorf("second backup not merged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(old, "backups")); !os.IsNotExist(err) {
		t.Error("old backups dir left behind")
	}

	// --help changes nothing on disk.
	os.WriteFile(filepath.Join(old, "checks.json"), []byte("{}"), 0o600)
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(old, "checks.json"), future, future)
	if err := run([]string{"--help"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(old, "checks.json")); err != nil {
		t.Error("--help moved files")
	}
}

func TestClientOnRestoresItsClusters(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("off", "acme/stage")
	if out := e.ok("off", "acme"); !strings.Contains(out, "acme: off (2 clusters)") {
		t.Errorf("off acme:\n%s", out)
	}
	if out := e.ok("on", "acme"); !strings.Contains(out, "still off: stage") {
		t.Errorf("on acme doesn't say stage stays off:\n%s", out)
	}
	e.WantContexts("acme/prod")
	// Not off as a whole now, so on means every cluster.
	e.ok("on", "acme")
	e.WantContexts("acme/prod", "acme/stage")
}

func TestDryRunTakesNoHandEdits(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	editTarget(t, e, func(cfg *api.Config) { delete(cfg.Contexts, "acme/stage") })
	state := filepath.Join(e.Home, "state.yaml")
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"rm", "acme/prod", "--dry-run"}, {"build", "--dry-run"}} {
		if out := e.ok(args...); strings.Contains(out, "turned off") {
			t.Errorf("kx %v synced:\n%s", args, out)
		}
		if after, _ := os.ReadFile(state); !bytes.Equal(before, after) {
			t.Errorf("kx %v changed state.yaml", args)
		}
	}
}

func TestDeletingTheOnlyContextTurnsItOff(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	editTarget(t, e, func(cfg *api.Config) { delete(cfg.Contexts, "acme/prod") }) // Lens "Remove"
	if out := e.ok("ls"); !strings.Contains(out, "acme/prod: removed from") {
		t.Errorf("deleting the last context was taken for a reset:\n%s", out)
	}
	e.ok("build")
	e.WantContexts()
}

func TestAddForceSaysReplaced(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "--name", "prod")
	e.ok("ns", "acme/prod", "monitoring")
	e.ok("off", "acme/prod")
	out := e.ok("add", e.Kubeconfig("b.yaml", "https://b"), "-c", "acme", "--name", "prod", "--force")
	if !strings.Contains(out, "replaced acme/prod") || !strings.Contains(out, "acme/prod stays off") {
		t.Errorf("add --force over a cluster that is off:\n%s", out)
	}
	if out := e.ok("ns", "acme/prod"); strings.TrimSpace(out) != "monitoring" {
		t.Errorf("namespace after add --force: %q", out)
	}
}

func TestParallelChangesAreNotLost(t *testing.T) {
	e := newEnv(t)
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, n := range names {
		e.ok("add", e.Kubeconfig(n+".yaml", "https://"+n), "-c", "acme", "--name", n)
		e.ok("off", "acme/"+n)
	}
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Go(func() {
			if err := run([]string{"on", "acme/" + n}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
				t.Errorf("on acme/%s: %v", n, err)
			}
		})
	}
	wg.Wait()
	e.WantContexts("acme/a", "acme/b", "acme/c", "acme/d", "acme/e", "acme/f")
}

func TestFailedWriteKeepsState(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "--name", "prod")
	dir := filepath.Dir(e.Target)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := e.run("", "off", "acme/prod")
	os.Chmod(dir, 0o700)
	if err == nil || strings.Contains(err.Error(), ".kx-") {
		t.Errorf("off with a read-only ~/.kube: %v", err)
	}
	if out := e.ok("ls"); strings.Contains(out, "off") {
		t.Errorf("state says off although the file still has it:\n%s", out)
	}
}

func TestBadValuesAreUsageErrors(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "--name", "prod")
	for _, args := range [][]string{
		{"add", e.Kubeconfig("b.yaml", "https://b"), "-c", "bad name"},
		{"off", "a b"},
		{"ns", "acme/prod", "Bad_NS"},
		{"help", "lss"},
	} {
		out, err := e.run("", args...)
		if code := report(err, io.Discard); code != 2 {
			t.Errorf("kx %v: exit %d, want 2 (%v)\n%s", args, code, err, out)
		}
	}
	if _, err := e.run("", "help", "lss"); err == nil || !strings.Contains(err.Error(), "lss") {
		t.Errorf("help lss: %v", err)
	}
}

func TestNamespaceOfACluster(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "--name", "prod")
	if out := e.ok("ns", "acme/prod"); strings.TrimSpace(out) != "default" {
		t.Errorf("ns acme/prod: %q", out)
	}
}

func TestWarningsStayOffStdout(t *testing.T) {
	e := newEnv(t)
	var out, errOut bytes.Buffer
	if err := run([]string{"-v"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "deprecated") || !strings.Contains(errOut.String(), "use --version") {
		t.Errorf("-v: stdout %q, stderr %q", out.String(), errOut.String())
	}
	_ = e
}

func TestExecMissingCommand(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "--name", "prod")
	out, err := e.run("", "exec", "acme", "--", "kx-no-such-command")
	if code := report(err, io.Discard); code != 127 || !strings.Contains(out, "command not found") {
		t.Errorf("exec of a missing command: exit %d\n%s", code, out)
	}
	entries, _ := os.ReadDir(filepath.Join(e.Home, "exec"))
	if len(entries) != 0 {
		t.Errorf("exec left %d files behind", len(entries))
	}
}

func TestUseUnknownName(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	if _, err := e.run("", "use", "nosuch"); err == nil || !strings.Contains(err.Error(), "no client or cluster named nosuch") {
		t.Errorf("unknown name: %v", err)
	}
	if _, err := e.run("", "use", "acme"); err == nil || !strings.Contains(err.Error(), "acme is a client") {
		t.Errorf("a client: %v", err)
	}
}

func TestImportCurrentDryRun(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	cfg, err := clientcmd.LoadFromFile(e.Target)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := clientcmd.LoadFromFile(e.Kubeconfig("eks.yaml", "https://eks"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clusters["shop"] = foreign.Clusters["kubernetes"]
	cfg.AuthInfos["shop"] = foreign.AuthInfos["kubernetes-admin"]
	ctx := foreign.Contexts["kubernetes-admin@kubernetes"]
	ctx.Cluster, ctx.AuthInfo = "shop", "shop"
	cfg.Contexts["shop"] = ctx
	if err := clientcmd.WriteToFile(*cfg, e.Target); err != nil {
		t.Fatal(err)
	}

	if out := e.ok("import-current", "--dry-run"); !strings.Contains(out, "would import shop as unsorted/shop") {
		t.Errorf("dry run said:\n%s", out)
	}
	e.WantContexts("acme/prod", "shop")
	if out := e.ok("ls"); strings.Contains(out, "unsorted") {
		t.Errorf("dry run imported:\n%s", out)
	}
}

// Ctrl-C during a check leaves the results of the check before it.
func TestInterruptedCheckKeepsResults(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.Kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.run("", "check")
	a, err := app.New(strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	before, err := a.LoadChecks()
	if err != nil || before["acme/prod"].Status == "" {
		t.Fatalf("no result to keep: %v, %v", before, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Check(ctx, []string{"acme/prod"}, false, time.Second, false); err == nil {
		t.Error("an interrupted check succeeded")
	}
	after, _ := a.LoadChecks()
	if !after["acme/prod"].CheckedAt.Equal(before["acme/prod"].CheckedAt) {
		t.Errorf("the interrupt replaced the result: %+v, was %+v", after["acme/prod"], before["acme/prod"])
	}
}
