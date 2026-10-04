package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/eugene-panin/kx/internal/app"
	"github.com/eugene-panin/kx/internal/probe"
	"github.com/eugene-panin/kx/internal/store"
	"github.com/eugene-panin/kx/internal/table"
	"k8s.io/client-go/tools/clientcmd"
)

type keyMap struct {
	Up, Down, Top, Bottom, PageUp, PageDown                                         key.Binding
	Use, Namespace, Toggle, Paste, Add, Check, Rename, Delete, Import, Filter, Quit key.Binding
	More                                                                            key.Binding
}

// helpLine is one line of key help. Not every key fits on a narrow terminal,
// so ? flips between two lines, and each ends with ? and q so those two are
// never cut off.
type helpLine []key.Binding

func (h helpLine) ShortHelp() []key.Binding  { return h }
func (h helpLine) FullHelp() [][]key.Binding { return [][]key.Binding{h} }

func (k keyMap) line(more bool) helpLine {
	if more {
		return helpLine{k.Namespace, k.Paste, k.Check, k.Rename, k.Import, k.More, k.Quit}
	}
	return helpLine{k.Use, k.Toggle, k.Add, k.Delete, k.Filter, k.More, k.Quit}
}

var keys = keyMap{
	Up:        key.NewBinding(key.WithKeys("up", "k")),
	Down:      key.NewBinding(key.WithKeys("down", "j")),
	Top:       key.NewBinding(key.WithKeys("home", "g")),
	Bottom:    key.NewBinding(key.WithKeys("end", "G")),
	PageUp:    key.NewBinding(key.WithKeys("pgup", "ctrl+b")),
	PageDown:  key.NewBinding(key.WithKeys("pgdown", "ctrl+f")),
	Use:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "use")),
	Namespace: key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "namespace")),
	Toggle:    key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "on/off")),
	Check:     key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "check")),
	Paste:     key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "paste")),
	Add:       key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add")),
	Rename:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rename")),
	Delete:    key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
	Import:    key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "import")),
	Filter:    key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Quit:      key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	More:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "more keys")),
}

// item is a line of the tree: a client header or one of its clusters.
type item struct {
	client string
	row    *app.ListRow // nil for a client header
}

func (it item) ref() string {
	if it.row == nil {
		return it.client
	}
	return it.row.Context
}

type mode int

const (
	modeNormal mode = iota
	modeFilter
	modeConfirm
	modeRename
	modeAddPath
	modeAddClient
	modeNamespace
)

type (
	loadedMsg struct {
		synced  []string // what Sync took from the target
		stamp   stamp
		rows    []app.ListRow
		checks  map[string]probe.Result
		st      *store.State
		foreign []string
		err     error
	}
	doneMsg struct {
		status string
		warn   string // what the command said on stderr, such as a lost current-context
		err    error
		added  []store.Ref // clusters to check and select once reloaded
	}
	checkMsg struct{ res probe.Result }
	watchMsg struct{}
	savedMsg struct{ err error }
)

// fixedLines is everything but the tree: title, column header, detail,
// status and help.
const fixedLines = 5

type model struct {
	a *app.App
	o *table.Output

	moreKeys bool // the help line shows the second set of keys

	rows    []app.ListRow
	st      *store.State
	foreign []string
	items   []item
	cursor  int
	offset  int
	width   int
	height  int

	mode    mode
	input   textinput.Model
	filter  string
	addPath string // pending add: a file...
	addData []byte // ...or a kubeconfig from the clipboard
	status  string
	failed  bool
	busy    bool
	notice  bool // status is a warning rather than a result

	stamp      stamp         // the target as last loaded
	watchEvery time.Duration // how often to look for hand edits of the target
	focus      string        // select this item after the next reload

	checks   map[string]probe.Result // this session's and remembered ones
	checking map[string]bool
	started  time.Time // results older than this come from the cache
	sem      chan struct{}
	timeout  time.Duration

	spin spinner.Model
	help help.Model

	clipboard func() ([]byte, error)
}

func newModel(a *app.App) model {
	in := textinput.New()
	in.CharLimit = 4096
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	return model{
		a:          a,
		o:          a.Output(a.Stdout),
		input:      in,
		checks:     map[string]probe.Result{},
		checking:   map[string]bool{},
		started:    time.Now(),
		sem:        make(chan struct{}, probe.Parallel),
		timeout:    probe.Timeout,
		spin:       sp,
		help:       help.New(),
		clipboard:  readClipboard,
		watchEvery: 2 * time.Second,
	}
}

// Run opens the interactive mode until the user quits.
func Run(a *app.App) error {
	stderr := os.Stderr
	// Exec auth plugins (aws, gke-gcloud-auth-plugin, ...) write straight to
	// os.Stderr and would garble the screen while checks run.
	if null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stderr = null
		defer func() {
			os.Stderr = stderr
			null.Close()
		}()
	}
	m := newModel(a)
	opts := []tea.ProgramOption{tea.WithInput(a.Stdin), tea.WithOutput(a.Stdout)}
	if !m.o.Color {
		// The help bar and the input bring their own colors; keep only
		// bold and reverse so the cursor stays visible.
		opts = append(opts, tea.WithColorProfile(colorprofile.Ascii))
	}
	p := tea.NewProgram(m, opts...)
	_, err := p.Run()
	return err
}

func (m model) Init() tea.Cmd { return tea.Batch(m.load(), m.watch()) }

// stamp identifies a version of the target file well enough to notice edits.
type stamp struct {
	mod  time.Time
	size int64
}

func stampOf(path string) stamp {
	fi, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	return stamp{fi.ModTime(), fi.Size()}
}

// watch schedules the next look at the target; zero watchEvery turns it off.
func (m model) watch() tea.Cmd {
	if m.watchEvery == 0 {
		return nil
	}
	return tea.Tick(m.watchEvery, func(time.Time) tea.Msg { return watchMsg{} })
}

// load takes hand edits of the target in, then reads everything shown.
func (m model) load() tea.Cmd {
	a := m.a
	return func() tea.Msg {
		notes, err := a.Sync()
		if err != nil {
			return loadedMsg{err: err}
		}
		rows, st, err := a.Rows()
		if err != nil {
			return loadedMsg{err: err}
		}
		checks, err := a.LoadChecks()
		if err != nil {
			return loadedMsg{err: err}
		}
		foreign, err := a.Unmanaged()
		return loadedMsg{rows: rows, checks: checks, st: st, foreign: foreign, synced: notes, stamp: stampOf(a.Target), err: err}
	}
}

// mutate runs a regular kx command with its output captured for the status line.
func (m model) mutate(fn func(q *app.App) error) (tea.Model, tea.Cmd) {
	return m.change(func(q *app.App) ([]store.Ref, error) { return nil, fn(q) })
}

// change is mutate for commands that add clusters: those get checked and
// selected once the tree reloads.
func (m model) change(fn func(q *app.App) ([]store.Ref, error)) (tea.Model, tea.Cmd) {
	if m.busy {
		return m, nil
	}
	m.busy = true
	a := m.a
	return m, func() tea.Msg {
		var out, warn bytes.Buffer
		q := *a
		q.Stdin, q.Stdout, q.Stderr = strings.NewReader(""), &out, &warn
		// The watch may not have seen the latest hand edit yet; the CLI
		// takes them in before every command, and so does this.
		if err := q.SyncAndReport(); err != nil {
			return doneMsg{err: err}
		}
		added, err := fn(&q)
		lines := strings.ReplaceAll(strings.TrimSpace(warn.String()), "kx: ", "")
		w := ""
		if lines != "" {
			w = summarize(lines)
		}
		return doneMsg{status: summarize(out.String()), warn: w, err: err, added: added}
	}
}

func summarize(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	first := strings.ReplaceAll(lines[0], "\t", "  ")
	if len(lines) > 1 {
		return fmt.Sprintf("%s (+%d more)", first, len(lines)-1)
	}
	return first
}

func (m model) probeCmd(r store.Ref) tea.Cmd {
	a, sem, timeout := m.a, m.sem, m.timeout
	return func() tea.Msg {
		sem <- struct{}{}
		defer func() { <-sem }()
		cfg, err := a.Store.Get(r)
		if err != nil {
			return checkMsg{probe.Result{Context: r.String(), Status: "error", Error: err.Error()}}
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return checkMsg{probe.Probe(ctx, cfg, r.String())}
	}
}

// saveChecks remembers this session's results for ls, check and the next start.
func (m model) saveChecks() tea.Cmd {
	var fresh []probe.Result
	for _, res := range m.checks {
		if res.CheckedAt.After(m.started) {
			fresh = append(fresh, res)
		}
	}
	a := m.a
	return func() tea.Msg { return savedMsg{a.SaveChecks(fresh)} }
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		m.clamp()
	case loadedMsg:
		if msg.err != nil {
			m.status, m.failed = firstLine(msg.err), true
			break
		}
		sel := m.selected()
		if m.focus != "" {
			sel, m.focus = m.focus, ""
		}
		m.rows, m.st, m.foreign, m.stamp = msg.rows, msg.st, msg.foreign, msg.stamp
		if len(msg.synced) > 0 {
			m.status, m.failed, m.notice = summarize(strings.Join(msg.synced, "\n")), false, true
		}
		for ctx, res := range msg.checks {
			if _, have := m.checks[ctx]; !have {
				m.checks[ctx] = res
			}
		}
		m.rebuild(sel)
	case doneMsg:
		m.busy = false
		m.status, m.failed = msg.status, false
		if msg.warn != "" {
			m.status, m.notice = msg.warn, true
		}
		if msg.err != nil {
			m.status, m.failed, m.notice = firstLine(msg.err), true, false
		}
		if len(msg.added) == 0 {
			return m, m.load()
		}
		m.focus = msg.added[0].String()
		return m, tea.Batch(m.load(), m.probeRefs(msg.added))
	case watchMsg:
		if !m.busy && stampOf(m.a.Target) != m.stamp {
			return m, tea.Batch(m.load(), m.watch())
		}
		return m, m.watch()
	case checkMsg:
		delete(m.checking, msg.res.Context)
		m.checks[msg.res.Context] = msg.res
		if len(m.checking) == 0 {
			return m, m.saveChecks()
		}
	case savedMsg:
		if msg.err != nil {
			m.status, m.failed = "save check results: "+firstLine(msg.err), true
		}
	case spinner.TickMsg:
		if len(m.checking) > 0 {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
	case tea.KeyPressMsg:
		if m.mode != modeNormal {
			return m.updateInput(msg)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m model) updateNormal(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.status, m.failed, m.notice = "", false, false
	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, keys.More):
		m.moreKeys = !m.moreKeys
		return m, nil
	case msg.String() == "esc":
		if m.filter != "" {
			m.filter = ""
			m.rebuild(m.selected())
		}
		return m, nil
	case key.Matches(msg, keys.Up):
		m.cursor--
	case key.Matches(msg, keys.Down):
		m.cursor++
	case key.Matches(msg, keys.Top):
		m.cursor = 0
	case key.Matches(msg, keys.Bottom):
		m.cursor = len(m.items) - 1
	case key.Matches(msg, keys.PageUp):
		m.cursor -= m.bodyHeight()
	case key.Matches(msg, keys.PageDown):
		m.cursor += m.bodyHeight()
	case key.Matches(msg, keys.Filter):
		return m.prompt(modeFilter, "/", m.filter)
	case key.Matches(msg, keys.Add):
		return m.prompt(modeAddPath, "kubeconfig file: ", "")
	case key.Matches(msg, keys.Paste):
		data, err := m.clipboard()
		if err != nil {
			m.status, m.failed = firstLine(err), true
			return m, nil
		}
		n, err := countContexts(data)
		if err != nil {
			m.status, m.failed = "clipboard holds no kubeconfig", true
			return m, nil
		}
		m.addPath, m.addData = "", data
		return m.prompt(modeAddClient, fmt.Sprintf("client for %d contexts from clipboard: ", n), m.currentClient())
	case key.Matches(msg, keys.Import):
		if len(m.foreign) == 0 {
			m.status = "nothing to import"
			return m, nil
		}
		return m.mutate(func(q *app.App) error { return q.ImportCurrent("unsorted") })
	case key.Matches(msg, keys.Check):
		return m.startChecks()
	}
	m.clamp()

	it, ok := m.current()
	if !ok {
		return m, nil
	}
	ref := it.ref()
	switch {
	case key.Matches(msg, keys.Use):
		if it.row == nil {
			m.status, m.failed = "pick a cluster of "+it.client, true
			return m, nil
		}
		return m.mutate(func(q *app.App) error { return q.Use(ref) })
	case key.Matches(msg, keys.Namespace):
		if it.row == nil {
			m.status, m.failed = "pick a cluster of "+it.client, true
			return m, nil
		}
		ns := it.row.Namespace
		if ns == "" {
			ns = "default"
		}
		return m.prompt(modeNamespace, "namespace for "+ref+": ", ns)
	case key.Matches(msg, keys.Toggle):
		on := !m.enabled(it)
		return m.mutate(func(q *app.App) error { return q.Toggle([]string{ref}, on) })
	case key.Matches(msg, keys.Rename):
		return m.prompt(modeRename, "rename "+ref+" → ", ref)
	case key.Matches(msg, keys.Delete):
		m.mode = modeConfirm
	}
	return m, nil
}

func (m model) prompt(md mode, prompt, value string) (tea.Model, tea.Cmd) {
	m.mode = md
	m.input.Prompt = prompt
	m.input.SetValue(value)
	m.input.CursorEnd()
	return m, m.input.Focus()
}

func (m model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeConfirm {
		m.mode = modeNormal
		if it, ok := m.current(); ok && msg.String() == "y" {
			ref := it.ref()
			return m.mutate(func(q *app.App) error { return q.Remove([]string{ref}, true, false) })
		}
		return m, nil
	}

	switch msg.String() {
	case "esc":
		if m.mode == modeFilter {
			m.filter = ""
			m.rebuild(m.selected())
		}
		m.mode = modeNormal
		m.input.Blur()
		return m, nil
	case "enter":
		md, val := m.mode, strings.TrimSpace(m.input.Value())
		m.mode = modeNormal
		m.input.Blur()
		switch md {
		case modeRename:
			it, ok := m.current()
			if !ok || val == "" || val == it.ref() {
				return m, nil
			}
			from := it.ref()
			return m.mutate(func(q *app.App) error { return q.Move(from, val) })
		case modeAddPath:
			if val == "" {
				return m, nil
			}
			path := expandHome(unquotePath(val))
			data, err := os.ReadFile(path)
			if err != nil {
				m.status, m.failed = firstLine(err), true
				return m, nil
			}
			n, err := countContexts(data)
			if err != nil {
				m.status, m.failed = path+" is not a kubeconfig", true
				return m, nil
			}
			m.addPath, m.addData = path, nil
			return m.prompt(modeAddClient, fmt.Sprintf("client for %d contexts from %s: ", n, filepath.Base(path)), m.currentClient())
		case modeNamespace:
			it, ok := m.current()
			if !ok || it.row == nil || val == "" {
				return m, nil
			}
			ref := it.ref()
			return m.mutate(func(q *app.App) error { return q.Namespace([]string{ref, val}) })
		case modeAddClient:
			if val == "" {
				return m, nil
			}
			path, data := m.addPath, m.addData
			return m.change(func(q *app.App) ([]store.Ref, error) {
				if data != nil {
					q.Stdin = bytes.NewReader(data)
					return q.Add("-", val, "", nil, false)
				}
				return q.Add(path, val, "", nil, false)
			})
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.mode == modeFilter {
		m.filter = m.input.Value()
		m.rebuild(m.selected())
	}
	return m, cmd
}

func (m model) startChecks() (tea.Model, tea.Cmd) {
	var refs []store.Ref
	for _, it := range m.items {
		if it.row != nil {
			refs = append(refs, store.Ref{Client: it.row.Client, Cluster: it.row.Cluster})
		}
	}
	return m, m.probeRefs(refs)
}

// probeRefs starts checks for the clusters not being checked already.
func (m model) probeRefs(refs []store.Ref) tea.Cmd {
	ticking := len(m.checking) > 0
	var cmds []tea.Cmd
	for _, r := range refs {
		if m.checking[r.String()] {
			continue
		}
		m.checking[r.String()] = true
		cmds = append(cmds, m.probeCmd(r))
	}
	if len(cmds) > 0 && !ticking {
		cmds = append(cmds, m.spin.Tick)
	}
	return tea.Batch(cmds...)
}

func (m model) currentClient() string {
	it, _ := m.current()
	return it.client
}

func countContexts(data []byte) (int, error) {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return 0, err
	}
	if len(cfg.Contexts) == 0 {
		return 0, errors.New("no contexts")
	}
	return len(cfg.Contexts), nil
}

func readClipboard() ([]byte, error) {
	for _, c := range [][]string{{"pbpaste"}, {"wl-paste", "--no-newline"}, {"xclip", "-o", "-selection", "clipboard"}, {"xsel", "-ob"}} {
		if _, err := exec.LookPath(c[0]); err == nil {
			return exec.Command(c[0], c[1:]...).Output()
		}
	}
	return nil, errors.New("no clipboard tool found (pbpaste, wl-paste, xclip, xsel)")
}

// unquotePath undoes what terminals do to a file dropped onto them: wrap it
// in quotes or escape spaces and other shell characters with backslashes.
func unquotePath(p string) string {
	if len(p) >= 2 && (p[0] == '\'' || p[0] == '"') && p[len(p)-1] == p[0] {
		return p[1 : len(p)-1]
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+1 < len(p) {
			i++
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

func (m model) enabled(it item) bool {
	if it.row != nil {
		return it.row.Enabled
	}
	return m.st == nil || m.st.Enabled(store.Ref{Client: it.client})
}

func (m model) current() (item, bool) {
	if m.cursor < 0 || m.cursor >= len(m.items) {
		return item{}, false
	}
	return m.items[m.cursor], true
}

func (m model) selected() string {
	if it, ok := m.current(); ok {
		return it.ref()
	}
	return ""
}

// rebuild applies the filter and keeps the cursor on sel when it survived.
func (m *model) rebuild(sel string) {
	f := strings.ToLower(m.filter)
	var items []item
	last := ""
	for i := range m.rows {
		r := &m.rows[i]
		if f != "" && !strings.Contains(strings.ToLower(r.Context+" "+r.Server), f) {
			continue
		}
		if r.Client != last {
			last = r.Client
			items = append(items, item{client: r.Client})
		}
		items = append(items, item{client: r.Client, row: r})
	}
	m.items = items
	for i, it := range items {
		if it.ref() == sel {
			m.cursor = i
			break
		}
	}
	m.clamp()
}

func (m model) bodyHeight() int { return max(1, m.height-fixedLines) }

func (m *model) clamp() {
	m.cursor = max(0, min(m.cursor, len(m.items)-1))
	h := m.bodyHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(0, min(m.offset, len(m.items)-h))
}

func (m model) View() tea.View {
	v := tea.NewView(m.screen())
	v.AltScreen = true
	return v
}

func (m model) screen() string {
	if m.width == 0 {
		return ""
	}
	o := *m.o
	// Without color the selection has no highlight, so it gets a gutter mark.
	gutter := !o.Color
	o.Width = m.width
	if gutter {
		o.Width -= 2
	}
	fitLine := func(s string) string { return ansi.Truncate(s, m.width-1, "…") }

	var b strings.Builder
	clients := 0
	for _, it := range m.items {
		if it.row == nil {
			clients++
		}
	}
	title := o.Paint(o.Title, "kx") + o.Paint(o.Dim, fmt.Sprintf("  %s · %s", countOf(len(m.rows), "cluster"), countOf(clients, "client")))
	if m.filter != "" && m.mode != modeFilter {
		title += o.Paint(o.Warn, "  /"+m.filter)
	}
	b.WriteString(fitLine(title) + "\n")

	cols := []table.Column{
		{},
		{Title: "CLUSTER", Shrink: 12},
		{Title: "SERVER", Shrink: 16, Trim: true, Drop: 2},
		{Title: "NAMESPACE", Drop: 1},
		{Title: "VERSION", Drop: 3},
		{Title: "STATE"},
		{Title: "CHECK", Drop: 4},
	}
	policy := probe.NewVersionPolicy(m.checks)
	rows := make([]table.Row, len(m.items))
	for i, it := range m.items {
		rows[i] = m.itemRow(&o, it, policy)
	}
	lines := o.Render(cols, rows)
	if gutter {
		for i := range lines {
			lines[i] = "  " + lines[i]
		}
	}
	b.WriteString(lines[0] + "\n")
	body := lines[1:]
	for i := m.offset; i < m.offset+m.bodyHeight(); i++ {
		switch {
		case len(body) == 0 && i == 0:
			hint := "no clusters yet, press a to add a kubeconfig"
			if m.filter != "" {
				hint = "nothing matches /" + m.filter
			}
			b.WriteString(o.Paint(o.Dim, fitLine(hint)))
		case i < len(body) && i == m.cursor && gutter:
			b.WriteString("> " + body[i][2:])
		case i < len(body) && i == m.cursor:
			plain := ansi.Strip(body[i])
			b.WriteString(o.Paint(o.Sel, plain+strings.Repeat(" ", max(0, m.width-1-ansi.StringWidth(plain)))))
		case i < len(body):
			b.WriteString(body[i])
		}
		b.WriteString("\n")
	}
	b.WriteString(fitLine(m.detail(&o)) + "\n")
	b.WriteString(fitLine(m.statusLine(&o)) + "\n")
	b.WriteString(fitLine(m.help.View(keys.line(m.moreKeys))))
	return b.String()
}

func (m model) itemRow(o *table.Output, it item, policy probe.VersionPolicy) table.Row {
	if it.row == nil {
		t := o.Paint(o.Title, it.client)
		if !m.enabled(it) {
			t += " " + o.Paint(o.Dim, "off")
		}
		return table.Row{Title: t}
	}
	r := it.row
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
	res, checked := m.checks[r.Context]
	version := app.VersionCell(o, r.Version, policy)
	if checked && res.Version != "" {
		version = app.VersionCell(o, res.Version, policy)
	}
	if !r.Enabled {
		version.Style = o.Dim
	}
	var check table.Cell
	switch {
	case m.checking[r.Context]:
		check = table.Cell{Text: ansi.Strip(m.spin.View()), Style: o.Dim}
	case checked:
		check = app.CheckCell(o, res)
		if !res.CheckedAt.After(m.started) {
			// Remembered from an earlier run: shown, but muted.
			check.Style = o.Dim
		}
	}
	return table.Row{Cells: []table.Cell{mark, name, {Text: r.Server, Style: base}, {Text: r.Namespace, Style: base}, version, state, check}}
}

func (m model) detail(o *table.Output) string {
	it, ok := m.current()
	if !ok {
		return ""
	}
	if it.row == nil {
		total, off := 0, 0
		for _, r := range m.rows {
			if r.Client == it.client {
				total++
				if !r.Enabled {
					off++
				}
			}
		}
		return o.Paint(o.Dim, fmt.Sprintf("%s: %d clusters, %d off", it.client, total, off))
	}
	r := it.row
	parts := []string{o.Paint(o.Bold, r.Context)}
	if res, ok := m.checks[r.Context]; ok {
		c := app.CheckCell(o, res)
		parts = append(parts, o.Paint(c.Style, c.Text))
		for _, p := range res.Problems(probe.NewVersionPolicy(m.checks)) {
			parts = append(parts, o.Paint(o.Bad, p))
		}
		if res.Nodes != nil {
			parts = append(parts, "nodes "+res.Nodes.String())
		}
		for _, p := range []string{res.Version, probe.ShortUser(res.User)} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if exp := probe.FormatExpiry(res.Expires, time.Now()); exp != "" {
			parts = append(parts, "expires "+exp)
		}
		parts = append(parts, o.Paint(o.Dim, "checked "+probe.FormatAge(time.Since(res.CheckedAt))))
	}
	parts = append(parts, o.Paint(o.Dim, r.Server))
	return strings.Join(parts, "  ")
}

func (m model) statusLine(o *table.Output) string {
	switch m.mode {
	case modeFilter, modeRename, modeAddPath, modeAddClient, modeNamespace:
		return m.input.View()
	case modeConfirm:
		it, _ := m.current()
		what := it.ref()
		if it.row == nil {
			n := 0
			for _, r := range m.rows {
				if r.Client == it.client {
					n++
				}
			}
			what = fmt.Sprintf("%s and its %d clusters", it.client, n)
		}
		return o.Paint(o.Warn, "delete "+what+"? y/N")
	}
	switch {
	case m.status != "" && m.failed:
		return o.Paint(o.Bad, m.status)
	case m.status != "" && m.notice:
		return o.Paint(o.Warn, m.status)
	case m.status != "":
		return o.Paint(o.OK, m.status)
	case len(m.foreign) > 0:
		return o.Paint(o.Warn, fmt.Sprintf("%d unmanaged contexts, i imports them into unsorted (%s)", len(m.foreign), m.a.TargetName()))
	}
	return ""
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
