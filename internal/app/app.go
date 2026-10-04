package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/eugene-panin/kx/internal/store"
	"github.com/eugene-panin/kx/internal/table"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// New reads KX_HOME and KX_KUBECONFIG and returns an App for them.
func New(stdin io.Reader, stdout, stderr io.Writer) (*App, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	xdg := func(env, def string) string {
		if d := os.Getenv(env); filepath.IsAbs(d) {
			return filepath.Join(d, "kx")
		}
		return filepath.Join(home, def, "kx")
	}
	// KX_HOME keeps everything in one place; otherwise clusters are config,
	// backups are state and check results are cache, each in its XDG home.
	dir := os.Getenv("KX_HOME")
	stateDir, cacheDir := dir, dir
	if dir == "" {
		dir = xdg("XDG_CONFIG_HOME", ".config")
		stateDir = xdg("XDG_STATE_HOME", filepath.Join(".local", "state"))
		cacheDir = xdg("XDG_CACHE_HOME", ".cache")
	}
	target := os.Getenv("KX_KUBECONFIG")
	if target == "" {
		target = filepath.Join(home, ".kube", "config")
	}
	if target, err = filepath.Abs(target); err != nil {
		return nil, err
	}
	// Write through a symlinked kubeconfig instead of replacing the link.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	return &App{Store: store.New(dir), Target: target, Stdin: stdin, Stdout: stdout, Stderr: stderr, stateDir: stateDir, cacheDir: cacheDir}, nil
}

// MoveOldFiles moves what kx before 0.6 kept next to the clusters to its XDG
// homes. Backups hold credentials and ~/.config is often synced as dotfiles,
// so they shouldn't stay there. It merges rather than skips, because an older
// kx may still write to the old place after the first move.
func (a *App) MoveOldFiles() {
	dir := a.Store.Dir()
	moved := false
	if old := filepath.Join(dir, "backups"); old != a.backupsDir() {
		entries, _ := os.ReadDir(old)
		for _, e := range entries {
			to := filepath.Join(a.backupsDir(), e.Name())
			if _, err := os.Stat(to); err == nil {
				continue
			}
			if os.MkdirAll(a.backupsDir(), 0o700) == nil && os.Rename(filepath.Join(old, e.Name()), to) == nil {
				moved = true
			}
		}
		os.Remove(old) // only succeeds once it's empty
		if moved {
			fmt.Fprintf(a.Stderr, "kx: moved backups to %s\n", tilde(a.backupsDir()))
		}
	}
	if old := filepath.Join(dir, "checks.json"); old != a.checksPath() {
		oldInfo, err := os.Stat(old)
		if err != nil {
			return
		}
		// Keep whichever is newer.
		if newInfo, err := os.Stat(a.checksPath()); err == nil && !oldInfo.ModTime().After(newInfo.ModTime()) {
			os.Remove(old)
			return
		}
		if os.MkdirAll(filepath.Dir(a.checksPath()), 0o700) == nil && os.Rename(old, a.checksPath()) == nil {
			fmt.Fprintf(a.Stderr, "kx: moved check results to %s\n", tilde(a.checksPath()))
		}
	}
}

func (a *App) backupsDir() string { return filepath.Join(a.stateDir, "backups") }

// Output is a table writer for w that honors --no-color.
func (a *App) Output(w io.Writer) *table.Output {
	o := table.New(w)
	if a.NoColor {
		o.Color = false
	}
	return o
}

// say prints a status line about what a command did; --quiet drops it.
func (a *App) say(format string, args ...any) {
	if !a.Quiet {
		fmt.Fprintf(a.Stdout, format+"\n", args...)
	}
}

const keepBackups = 10

// App carries out kx commands. The CLI and the interactive mode share it.
type App struct {
	Store  *store.Store
	Target string // the kubeconfig kx builds
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Set from the global flags.
	NoColor bool // --no-color
	NoInput bool // --no-input: never ask, fail instead
	Quiet   bool // --quiet: no status lines

	stateDir   string // backups, per XDG state
	cacheDir   string // check results, per XDG cache
	lastBackup string // where the last Build put the file it replaced, if anywhere
}

func (a *App) loadTarget() (*api.Config, error) {
	cfg, err := clientcmd.LoadFromFile(a.Target)
	if errors.Is(err, fs.ErrNotExist) {
		return api.NewConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s: %w\nfix it by hand, or write it anew with `kx build --force` (previous versions are in %s)",
			a.TargetName(), err, tilde(a.backupsDir()))
	}
	return cfg, nil
}

// lock serializes changes across kx processes: the interactive mode and a
// command in another terminal, or an agent running several at once. Without it
// two read-modify-write cycles of state.yaml lose one of the changes.
func (a *App) lock() (unlock func(), err error) {
	dir := a.Store.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() { f.Close() }, nil
}

// Unmanaged returns contexts in the target kubeconfig that kx neither stores
// nor generated last time: a rebuild would silently drop them.
func (a *App) Unmanaged() ([]string, error) {
	cur, err := a.loadTarget()
	if err != nil {
		return nil, err
	}
	refs, err := a.Store.Clusters()
	if err != nil {
		return nil, err
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range store.ContextNames(cur) {
		known := slices.Contains(st.Generated, name) ||
			slices.ContainsFunc(refs, func(r store.Ref) bool { return r.String() == name })
		if !known {
			out = append(out, name)
		}
	}
	return out, nil
}

// checkTarget refuses to proceed while the target holds contexts kx would lose.
func (a *App) checkTarget() error {
	names, err := a.Unmanaged()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	return fmt.Errorf("%s has contexts not managed by kx:\n  %s\n"+
		"take them over with `kx import-current -c <client>` or drop them with `kx build --force`",
		a.Target, strings.Join(names, "\n  "))
}

// prepare runs before any change: it refuses to touch a target holding
// foreign contexts. Hand edits are taken in once per command, before it runs
// (SyncAndReport), not here, so a dry run never writes.
func (a *App) prepare() error {
	return a.checkTarget()
}

// SyncAndReport runs Sync and tells about the changes on Stderr.
func (a *App) SyncAndReport() error {
	notes, err := a.Sync()
	for _, n := range notes {
		fmt.Fprintln(a.Stderr, "kx:", n)
	}
	return err
}

// build regenerates the target. renames maps old context names to new ones so
// that a renamed current-context survives.
func (a *App) build(force bool, renames map[string]string) error {
	if !force {
		if err := a.checkTarget(); err != nil {
			return err
		}
	}
	refs, err := a.Store.Clusters()
	if err != nil {
		return err
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return err
	}
	var (
		parts []*api.Config
		names []string
	)
	for _, r := range refs {
		if !st.Enabled(r) {
			continue
		}
		cfg, err := a.Store.Get(r)
		if err != nil {
			return err
		}
		parts = append(parts, cfg)
		names = append(names, store.ContextNames(cfg)...)
	}
	out := store.Merge(parts)
	var previous string
	// A broken target is only possible with --force; it just loses current-context then.
	if cur, err := a.loadTarget(); err == nil {
		previous = cur.CurrentContext
		name := cur.CurrentContext
		if n, ok := renames[name]; ok {
			name = n
		}
		if _, ok := out.Contexts[name]; ok {
			out.CurrentContext = name
		}
	}
	data, err := clientcmd.Write(*out)
	if err != nil {
		return err
	}
	a.lastBackup = ""
	old, err := os.ReadFile(a.Target)
	switch {
	case err == nil && bytes.Equal(old, data):
	case err == nil:
		if a.lastBackup, err = a.backup(old); err != nil {
			return fmt.Errorf("backup %s: %w", a.Target, err)
		}
		fallthrough
	case errors.Is(err, fs.ErrNotExist):
		if err := store.WriteFile(a.Target, data); err != nil {
			return err
		}
	default:
		return err
	}
	st.Generated = names
	if err := a.Store.SaveState(st); err != nil {
		return err
	}
	if previous != "" && out.CurrentContext == "" {
		// kubectl would now fail with "no context"; say why.
		fmt.Fprintf(a.Stderr, "kx: current-context %s is off or gone, none is set now; pick one with kx use\n", previous)
	}
	return nil
}

// backup keeps data as the newest backup and returns its path.
func (a *App) backup(data []byte) (string, error) {
	dir := a.backupsDir()
	path := filepath.Join(dir, "config-"+time.Now().Format("20060102-150405.000000"))
	if err := store.WriteFile(path, data); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var backups []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config-") {
			backups = append(backups, e.Name())
		}
	}
	slices.Sort(backups)
	for len(backups) > keepBackups {
		if err := os.Remove(filepath.Join(dir, backups[0])); err != nil {
			return "", err
		}
		backups = backups[1:]
	}
	return path, nil
}

// Rebuild is `kx build`: regenerate the target and say what happened,
// including the foreign contexts --force drops. dryRun says what would happen
// and writes nothing.
func (a *App) Rebuild(force, dryRun bool) error {
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	var dropped []string
	if force {
		// Best effort: an unreadable target has nothing to list.
		dropped, _ = a.Unmanaged()
	} else if err := a.checkTarget(); err != nil {
		return err
	}
	if dryRun {
		n, err := a.enabledCount()
		if err != nil {
			return err
		}
		for _, d := range dropped {
			fmt.Fprintf(a.Stdout, "would drop %s\n", d)
		}
		fmt.Fprintf(a.Stdout, "would write %s: %s\n", a.TargetName(), plural(n, "cluster"))
		return nil
	}
	if err := a.build(force, nil); err != nil {
		return err
	}
	for _, d := range dropped {
		a.say("dropped %s", d)
	}
	cfg, err := a.loadTarget()
	if err != nil {
		return err
	}
	a.say("wrote %s: %s", a.TargetName(), plural(len(cfg.Contexts), "cluster"))
	if a.lastBackup != "" {
		a.say("previous version: %s", tilde(a.lastBackup))
	}
	return nil
}

func (a *App) enabledCount() (int, error) {
	refs, err := a.Store.Clusters()
	if err != nil {
		return 0, err
	}
	st, err := a.Store.LoadState()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range refs {
		if st.Enabled(r) {
			n++
		}
	}
	return n, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
