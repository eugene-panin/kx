package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/client-go/tools/clientcmd"
)

// drive feeds msgs to the model and runs the commands it returns until
// nothing is left, skipping spinner ticks so tests do not wait on timers.
func drive(t *testing.T, m tea.Model, msgs ...tea.Msg) tea.Model {
	t.Helper()
	queue := msgs
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1000 {
			t.Fatal("model does not settle")
		}
		msg := queue[0]
		queue = queue[1:]
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c != nil {
					queue = append(queue, c())
				}
			}
			continue
		}
		var cmd tea.Cmd
		m, cmd = m.Update(msg)
		if _, tick := msg.(spinner.TickMsg); tick || cmd == nil {
			continue
		}
		queue = append(queue, cmd())
	}
	return m
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func typed(s string) []tea.Msg {
	var msgs []tea.Msg
	for _, r := range s {
		msgs = append(msgs, keyMsg(string(r)))
	}
	return msgs
}

func newTUI(t *testing.T, e *env, width, height int) tea.Model {
	t.Helper()
	a, err := newApp(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return startTUI(t, newModel(a), width, height)
}

func startTUI(t *testing.T, m model, width, height int) tea.Model {
	t.Helper()
	return drive(t, m, m.Init()(), tea.WindowSizeMsg{Width: width, Height: height})
}

func selectedRef(m tea.Model) string { return m.(model).selected() }

func TestTUINavigateAndToggle(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	e.ok("add", e.kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")

	m := newTUI(t, e, 100, 20)
	if got := selectedRef(m); got != "acme" {
		t.Fatalf("starts on %q, want acme", got)
	}
	m = drive(t, m, keyMsg("j"), keyMsg("space"))
	if got := selectedRef(m); got != "acme/prod" {
		t.Fatalf("cursor on %q after toggle, want acme/prod", got)
	}
	e.wantContexts("acme/stage", "globex/main")

	// Toggling a client header switches the whole client.
	m = drive(t, m, keyMsg("G"), keyMsg("k"), keyMsg("space"))
	if got := selectedRef(m); got != "globex" {
		t.Fatalf("cursor on %q, want globex", got)
	}
	e.wantContexts("acme/stage")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "globex off") || !strings.Contains(v, "off") {
		t.Errorf("view does not show disabled state:\n%s", v)
	}
}

func TestTUIUse(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("b.yaml", "https://b"), "-c", "acme", "-n", "stage")
	m := newTUI(t, e, 100, 20)

	m = drive(t, m, keyMsg("enter"))
	if st := m.(model).status; !strings.Contains(st, "pick a cluster") {
		t.Errorf("enter on a client: status = %q", st)
	}
	m = drive(t, m, keyMsg("G"), keyMsg("enter"))
	if cfg, _ := clientcmd.LoadFromFile(e.target); cfg.CurrentContext != "acme/stage" {
		t.Errorf("current-context = %q", cfg.CurrentContext)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "*  stage") {
		t.Errorf("view does not mark the current cluster:\n%s", v)
	}
}

func TestTUIFilterRenameDelete(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	e.ok("add", e.kubeconfig("c.yaml", "https://c"), "-c", "globex", "-n", "main")
	m := newTUI(t, e, 100, 20)

	m = drive(t, m, append([]tea.Msg{keyMsg("/")}, typed("glob")...)...)
	if items := m.(model).items; len(items) != 2 || items[0].client != "globex" {
		t.Fatalf("filtered items = %+v", items)
	}
	m = drive(t, m, keyMsg("enter"), keyMsg("j"))
	if got := selectedRef(m); got != "globex/main" {
		t.Fatalf("selected %q", got)
	}

	rename := append([]tea.Msg{keyMsg("r"), keyMsg("ctrl+u")}, typed("globex/prod")...)
	m = drive(t, m, append(rename, keyMsg("enter"))...)
	e.wantContexts("acme/prod", "globex/prod")
	if got := selectedRef(m); got != "globex/prod" {
		t.Errorf("cursor after rename on %q, want globex/prod", got)
	}

	m = drive(t, m, keyMsg("d"), keyMsg("n"))
	e.wantContexts("acme/prod", "globex/prod")
	m = drive(t, m, keyMsg("d"), keyMsg("y"))
	e.wantContexts("acme/prod")

	m = drive(t, m, keyMsg("esc"))
	if items := m.(model).items; len(items) != 2 {
		t.Errorf("items after clearing the filter = %+v", items)
	}
}

func TestTUIAdd(t *testing.T) {
	e := newEnv(t)
	path := e.kubeconfig("a.yaml", "https://a")
	m := newTUI(t, e, 100, 20)
	if v := m.View(); !strings.Contains(v, "press a to add") {
		t.Errorf("empty view has no hint:\n%s", v)
	}
	msgs := append([]tea.Msg{keyMsg("a")}, typed(path)...)
	msgs = append(msgs, keyMsg("enter"))
	msgs = append(msgs, typed("acme")...)
	m = drive(t, m, append(msgs, keyMsg("enter"))...)
	e.wantContexts("acme/kubernetes-admin-kubernetes")
	if st := m.(model).status; !strings.Contains(st, "added acme/kubernetes-admin-kubernetes") {
		t.Errorf("status = %q", st)
	}
}

func TestTUIPaste(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://a"), "-c", "acme", "-n", "prod")
	a, err := newApp(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(a)
	clip := []byte("not a kubeconfig")
	m.clipboard = func() ([]byte, error) { return clip, nil }
	tm := startTUI(t, m, 100, 20)

	tm = drive(t, tm, keyMsg("p"))
	if st := tm.(model).status; st != "clipboard holds no kubeconfig" {
		t.Fatalf("status = %q", st)
	}

	clip = []byte(strings.Replace(strings.Replace(kubeadm, "%s", "https://new", 1), "certificate-authority: ca.crt", "insecure-skip-tls-verify: true", 1))
	tm = drive(t, tm, keyMsg("p"))
	if v := tm.View(); !strings.Contains(v, "client for 1 contexts from clipboard: acme") {
		t.Fatalf("no client prompt prefilled with the selected client:\n%s", v)
	}
	msgs := append([]tea.Msg{keyMsg("ctrl+u")}, typed("globex")...)
	drive(t, tm, append(msgs, keyMsg("enter"))...)
	e.wantContexts("acme/prod", "globex/kubernetes-admin-kubernetes")
}

func TestTUIAddDroppedFile(t *testing.T) {
	e := newEnv(t)
	src := e.kubeconfig("a.yaml", "https://a")
	dropped := filepath.Join(e.src, "my config.yaml")
	if err := os.Rename(src, dropped); err != nil {
		t.Fatal(err)
	}
	for _, typedPath := range []string{
		strings.ReplaceAll(dropped, " ", `\ `), // iTerm, Terminal.app
		"'" + dropped + "'",                    // some terminals quote instead
	} {
		m := newTUI(t, e, 100, 20)
		msgs := append([]tea.Msg{keyMsg("a")}, typed(typedPath)...)
		msgs = append(msgs, keyMsg("enter"))
		msgs = append(msgs, typed("acme")...)
		m = drive(t, m, append(msgs, keyMsg("enter"))...)
		if st := m.(model).status; !strings.Contains(st, "acme/kubernetes-admin-kubernetes") {
			t.Errorf("%s: status = %q", typedPath, st)
		}
		e.ok("rm", "acme", "-y")
	}
}

func TestTUICheck(t *testing.T) {
	e := newEnv(t)
	srv := fakeAPI(t, apiOpts{})
	writeCA(t, e, srv)
	e.ok("add", e.kubeconfig("a.yaml", srv.URL), "-c", "acme", "-n", "prod")
	m := newTUI(t, e, 120, 20)
	m = drive(t, m, keyMsg("c"), keyMsg("j"))
	res, ok := m.(model).checks["acme/prod"]
	if !ok || res.Status != "ok" {
		t.Fatalf("check result = %+v", res)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "kubernetes-admin") || !strings.Contains(v, "v1.31.2") {
		t.Errorf("detail line misses the check result:\n%s", v)
	}
}

func TestTUIRemembersChecks(t *testing.T) {
	e := newEnv(t)
	srv := fakeAPI(t, apiOpts{version: "v1.36.2", nodes: []string{"Ready"}})
	writeCA(t, e, srv)
	e.ok("add", e.kubeconfig("a.yaml", srv.URL), "-c", "acme", "-n", "prod")
	e.ok("check")
	srv.Close()

	// A new session shows the last result without probing anything.
	m := newTUI(t, e, 140, 20)
	m = drive(t, m, keyMsg("j"))
	v := ansi.Strip(m.View())
	for _, want := range []string{"v1.36.2", "ok", "nodes 1/1", "checked just now"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}

	// A fresh check replaces it and is saved for the next start.
	m = drive(t, m, keyMsg("c"))
	if res := m.(model).checks["acme/prod"]; res.Status != "unreachable" {
		t.Fatalf("fresh result = %+v", res)
	}
	checks, err := (&store{dir: e.home}).loadChecks()
	if err != nil || checks["acme/prod"].Status != "unreachable" {
		t.Errorf("saved result = %+v, %v", checks["acme/prod"], err)
	}
}

func TestTUIForeignContexts(t *testing.T) {
	e := newEnv(t)
	cfg, err := clientcmd.LoadFromFile(e.kubeconfig("a.yaml", "https://a"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clusters["kubernetes"].CertificateAuthority, cfg.Clusters["kubernetes"].CertificateAuthorityData = "", []byte("CA")
	if err := clientcmd.WriteToFile(*cfg, e.target); err != nil {
		t.Fatal(err)
	}
	m := newTUI(t, e, 120, 20)
	if v := m.View(); !strings.Contains(v, "unmanaged contexts") {
		t.Errorf("no banner about foreign contexts:\n%s", v)
	}
	m = drive(t, m, keyMsg("i"))
	e.wantContexts("unsorted/kubernetes-admin-kubernetes")
	if v := m.View(); strings.Contains(v, "unmanaged contexts") {
		t.Errorf("banner still shown after import:\n%s", v)
	}
}

func TestTUIFitsAnyWidth(t *testing.T) {
	e := newEnv(t)
	e.ok("add", e.kubeconfig("a.yaml", "https://k8s.some-really-long-domain.example.com:6443"), "-c", "a-client-with-a-long-name")
	for _, w := range []int{30, 50, 80, 140} {
		for _, h := range []int{3, 8, 30} {
			m := newTUI(t, e, w, h)
			v := m.View()
			lines := strings.Split(v, "\n")
			if len(lines) > max(h, fixedLines+1) {
				t.Errorf("%dx%d: %d lines", w, h, len(lines))
			}
			for _, l := range lines {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("%dx%d: line %d wide: %q", w, h, lw, ansi.Strip(l))
				}
			}
		}
	}
}
