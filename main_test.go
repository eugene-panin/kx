package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eugene-panin/kx/internal/app"
	"github.com/eugene-panin/kx/internal/kxtest"
	"github.com/eugene-panin/kx/internal/probe"
	"github.com/eugene-panin/kx/internal/store"
	"k8s.io/client-go/tools/clientcmd"
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

	if _, err := e.run("n\n", "rm", "acme"); err != nil {
		t.Fatal(err)
	}
	if out := e.ok("ls", "acme"); !strings.Contains(out, "stage") {
		t.Fatalf("declined rm removed clusters:\n%s", out)
	}
	if _, err := e.run("", "rm", "acme"); err == nil {
		t.Error("rm of several clusters without confirmation succeeded")
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
	if out := e.ok("ls"); !strings.Contains(out, "*  acme    prod") {
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
	stored := filepath.Join(e.Home, "clusters", "acme", "badtoken.yaml")
	cfg, err := clientcmd.LoadFromFile(stored)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthInfos["acme/badtoken"].Token = "wrong"
	if err := clientcmd.WriteToFile(*cfg, stored); err != nil {
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
