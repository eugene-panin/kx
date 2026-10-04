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

	"k8s.io/client-go/tools/clientcmd"
)

// kubeadm-style kubeconfig: every cluster built this way has the same
// cluster/user/context names, which is what kx exists to untangle.
const kubeadm = `apiVersion: v1
kind: Config
clusters:
- name: kubernetes
  cluster:
    server: %s
    certificate-authority: ca.crt
users:
- name: kubernetes-admin
  user:
    token: secret
contexts:
- name: kubernetes-admin@kubernetes
  context:
    cluster: kubernetes
    user: kubernetes-admin
current-context: kubernetes-admin@kubernetes
`

type env struct {
	t      *testing.T
	home   string
	target string
	src    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, home: filepath.Join(dir, "kx"), target: filepath.Join(dir, "kube", "config"), src: filepath.Join(dir, "src")}
	t.Setenv("KX_HOME", e.home)
	t.Setenv("KX_KUBECONFIG", e.target)
	if err := os.MkdirAll(e.src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.src, "ca.crt"), []byte("CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) kubeconfig(name, server string) string {
	e.t.Helper()
	p := filepath.Join(e.src, name)
	if err := os.WriteFile(p, []byte(strings.Replace(kubeadm, "%s", server, 1)), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

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

func (e *env) contexts() []string {
	e.t.Helper()
	cfg, err := clientcmd.LoadFromFile(e.target)
	if err != nil {
		e.t.Fatal(err)
	}
	return contextNames(cfg)
}

func (e *env) wantContexts(want ...string) {
	e.t.Helper()
	if got := e.contexts(); !slices.Equal(got, want) {
		e.t.Errorf("contexts = %v, want %v", got, want)
	}
}

func TestAddResolvesNameClashes(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a:6443"), "-c", "acme", "--name", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b:6443"), "-c", "acme", "--name", "stage")
	e.ok("add", e.kubeconfig("c.yaml", "https://c:6443"), "-c", "globex")

	e.wantContexts("acme/prod", "acme/stage", "globex/kubernetes-admin-kubernetes")

	cfg, err := clientcmd.LoadFromFile(e.target)
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

	if _, err := e.run("", "add", e.kubeconfig("d.yaml", "https://d:6443"), "-c", "acme", "--name", "prod"); err == nil {
		t.Error("adding an existing cluster without --force succeeded")
	}
	e.ok("add", e.src+"/d.yaml", "-c", "acme", "--name", "prod", "--force")
	cfg, _ = clientcmd.LoadFromFile(e.target)
	if s := cfg.Clusters["acme/prod"].Server; s != "https://d:6443" {
		t.Errorf("after --force server = %s", s)
	}
}

func TestAddFromStdin(t *testing.T) {
	e := newEnv(t)
	t.Chdir(e.src)
	cfg := strings.Replace(kubeadm, "%s", "https://x:6443", 1)
	if out, err := e.run(cfg, "add", "-", "-c", "acme", "-n", "prod"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	e.wantContexts("acme/prod")
}

func TestOnOff(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("add", e.kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")

	e.ok("off", "acme")
	e.wantContexts("globex/main")

	e.ok("on", "acme/stage")
	e.wantContexts("acme/stage", "globex/main")

	e.ok("off", "globex/main")
	e.wantContexts("acme/stage")

	e.ok("on", "acme", "globex")
	e.wantContexts("acme/prod", "acme/stage", "globex/main")

	if out := e.ok("ls"); !strings.Contains(out, "acme") || strings.Contains(out, "off") {
		t.Errorf("ls after enabling everything:\n%s", out)
	}
	if _, err := e.run("", "off", "nope"); err == nil {
		t.Error("off on unknown client succeeded")
	}
}

func TestMove(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "unsorted")
	e.ok("off", "unsorted")

	e.ok("mv", "unsorted/kubernetes-admin-kubernetes", "acme")
	e.ok("mv", "acme/kubernetes-admin-kubernetes", "acme/prod")
	e.ok("on", "acme")
	e.wantContexts("acme/prod")

	cfg, err := clientcmd.LoadFromFile(e.target)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CurrentContext = "acme/prod"
	if err := clientcmd.WriteToFile(*cfg, e.target); err != nil {
		t.Fatal(err)
	}
	e.ok("mv", "acme", "acme-corp")
	e.wantContexts("acme-corp/prod")
	if cfg, _ = clientcmd.LoadFromFile(e.target); cfg.CurrentContext != "acme-corp/prod" {
		t.Errorf("current-context = %q after renaming it", cfg.CurrentContext)
	}

	cfg, err = clientcmd.LoadFromFile(filepath.Join(e.home, "clusters", "acme-corp", "prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := cfg.Contexts["acme-corp/prod"]
	if ctx == nil || ctx.Cluster != "acme-corp/prod" || ctx.AuthInfo != "acme-corp/prod" || cfg.CurrentContext != "acme-corp/prod" {
		t.Errorf("names not rewritten: %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(e.home, "clusters", "unsorted")); !os.IsNotExist(err) {
		t.Error("empty client directory left behind")
	}
}

func TestMovedDisabledClusterStaysOff(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "unsorted", "-n", "x")
	e.ok("off", "unsorted")
	e.ok("mv", "unsorted/x", "acme")
	e.wantContexts()
	if out := e.ok("ls"); !strings.Contains(out, "off") {
		t.Errorf("moved cluster lost its off state:\n%s", out)
	}
}

func TestRemove(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("add", e.kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")
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
	e.wantContexts("globex/main")

	// A client re-added under the same name must not inherit the old off state.
	e.ok("add", e.src+"/a.yaml", "-c", "acme", "-n", "prod")
	e.wantContexts("acme/prod", "globex/main")
}

func TestForeignContextsAreProtected(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")

	// Something like `aws eks update-kubeconfig` writes straight into the target.
	cfg, err := clientcmd.LoadFromFile(e.target)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := clientcmd.LoadFromFile(e.kubeconfig("eks.yaml", "https://eks"))
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
	if err := clientcmd.WriteToFile(*cfg, e.target); err != nil {
		t.Fatal(err)
	}

	out, err := e.run("", "off", "acme")
	if err == nil || !strings.Contains(err.Error(), eks) {
		t.Fatalf("off with a foreign context: err = %v\n%s", err, out)
	}
	e.wantContexts("acme/prod", eks)

	e.ok("import-current", "-c", "globex")
	e.wantContexts("acme/prod", "globex/shop")
	if cfg, _ = clientcmd.LoadFromFile(e.target); cfg.CurrentContext != "globex/shop" {
		t.Errorf("current-context = %q after import", cfg.CurrentContext)
	}
	e.ok("off", "acme")
	e.wantContexts("globex/shop")

	backups, err := os.ReadDir(filepath.Join(e.home, "backups"))
	if err != nil || len(backups) == 0 {
		t.Errorf("no backups written: %v", err)
	}
}

func TestBuildKeepsCurrentContext(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")

	cfg, _ := clientcmd.LoadFromFile(e.target)
	cfg.CurrentContext = "acme/stage"
	if err := clientcmd.WriteToFile(*cfg, e.target); err != nil {
		t.Fatal(err)
	}
	e.ok("off", "acme/prod")
	if cfg, _ = clientcmd.LoadFromFile(e.target); cfg.CurrentContext != "acme/stage" {
		t.Errorf("current-context = %q, want acme/stage", cfg.CurrentContext)
	}
	e.ok("off", "acme/stage")
	if cfg, _ = clientcmd.LoadFromFile(e.target); cfg.CurrentContext != "" {
		t.Errorf("current-context = %q after disabling it", cfg.CurrentContext)
	}
}

func TestNamespaceSwitchSurvivesRebuild(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "globex", "-n", "main")

	// What kubens / k9s do when switching namespace.
	cfg, _ := clientcmd.LoadFromFile(e.target)
	cfg.Contexts["acme/prod"].Namespace = "monitoring"
	if err := clientcmd.WriteToFile(*cfg, e.target); err != nil {
		t.Fatal(err)
	}

	if out := e.ok("ls", "acme"); !strings.Contains(out, "monitoring") {
		t.Errorf("ls before any rebuild:\n%s", out)
	}
	e.ok("off", "globex")
	e.ok("off", "acme")
	e.ok("on", "acme")
	if cfg, _ = clientcmd.LoadFromFile(e.target); cfg.Contexts["acme/prod"].Namespace != "monitoring" {
		t.Errorf("namespace = %q, want monitoring", cfg.Contexts["acme/prod"].Namespace)
	}
	if out := e.ok("ls"); !strings.Contains(out, "monitoring") {
		t.Errorf("ls does not show the switched namespace:\n%s", out)
	}
}

func TestExport(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("off", "acme/stage")

	cfg, err := clientcmd.Load([]byte(e.ok("export", "acme")))
	if err != nil {
		t.Fatal(err)
	}
	if got := contextNames(cfg); !slices.Equal(got, []string{"acme/prod", "acme/stage"}) {
		t.Errorf("exported contexts = %v", got)
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"kubernetes-admin@kubernetes":            "kubernetes-admin-kubernetes",
		"arn:aws:eks:eu-west-1:123:cluster/shop": "shop",
		"gke_proj_europe-west1_main":             "gke_proj_europe-west1_main",
		"@@@":                                    "cluster",
		"trailing/":                              "trailing",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListJSON(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "globex", "-n", "main")
	e.ok("off", "globex")

	var rows []listRow
	out := e.ok("ls", "--json")
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []listRow{
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
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("add", e.kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")
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
	if got := contextNames(cfg); !slices.Equal(got, []string{"acme/prod", "acme/stage"}) {
		t.Errorf("exec sees contexts %v", got)
	}
	if cfg.CurrentContext != "acme/prod" {
		t.Errorf("current-context = %q", cfg.CurrentContext)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp kubeconfig %s left behind", path)
	}

	_, err = e.run("", "exec", "globex/main", "--", "sh", "-c", "exit 3")
	if code, ok := errors.AsType[exitError](err); !ok || code != 3 {
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
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("off", "acme/stage")
	backups := func() int {
		entries, _ := os.ReadDir(filepath.Join(e.home, "backups"))
		return len(entries)
	}
	before := backups()

	if out := e.ok("use"); strings.TrimSpace(out) != "no current context" {
		t.Errorf("use without current = %q", out)
	}
	e.ok("use", "acme/prod")
	if cfg, _ := clientcmd.LoadFromFile(e.target); cfg.CurrentContext != "acme/prod" {
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
	if cfg, _ := clientcmd.LoadFromFile(e.target); cfg.CurrentContext != "" {
		t.Errorf("current-context = %q after disabling it", cfg.CurrentContext)
	}
}
