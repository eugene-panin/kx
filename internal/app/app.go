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
	"time"

	"github.com/eugene-panin/kx/internal/store"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// New reads KX_HOME and KX_KUBECONFIG and returns an App for them.
func New(stdin io.Reader, stdout, stderr io.Writer) (*App, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("KX_HOME")
	if dir == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		dir = filepath.Join(base, "kx")
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
	return &App{Store: store.New(dir), Target: target, Stdin: stdin, Stdout: stdout, Stderr: stderr}, nil
}

const keepBackups = 10

// App carries out kx commands. The CLI and the interactive mode share it.
type App struct {
	Store  *store.Store
	Target string // the kubeconfig kx builds
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func (a *App) loadTarget() (*api.Config, error) {
	cfg, err := clientcmd.LoadFromFile(a.Target)
	if errors.Is(err, fs.ErrNotExist) {
		return api.NewConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", a.Target, err)
	}
	return cfg, nil
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

// prepare runs before any change: it refuses to touch a target holding foreign
// contexts and pulls namespace switches made by other tools into the store.
func (a *App) prepare() error {
	if err := a.checkTarget(); err != nil {
		return err
	}
	return a.syncNamespaces()
}

// syncNamespaces copies namespaces switched in the target (k9s, kubens,
// kubectl config set-context) back into the store, so a rebuild keeps them
// and they survive off/on.
func (a *App) syncNamespaces() error {
	cur, err := a.loadTarget()
	if err != nil {
		return err
	}
	refs, err := a.Store.Clusters()
	if err != nil {
		return err
	}
	for _, r := range refs {
		tc := cur.Contexts[r.String()]
		if tc == nil {
			continue
		}
		cfg, err := a.Store.Get(r)
		if err != nil {
			return err
		}
		sc := cfg.Contexts[r.String()]
		if sc == nil || sc.Namespace == tc.Namespace {
			continue
		}
		sc.Namespace = tc.Namespace
		if err := a.Store.Put(r, cfg); err != nil {
			return err
		}
	}
	return nil
}

// Build regenerates the target. renames maps old context names to new ones so
// that a renamed current-context survives.
func (a *App) Build(force bool, renames map[string]string) error {
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
	// A broken target is only possible with --force; it just loses current-context then.
	if cur, err := a.loadTarget(); err == nil {
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
	old, err := os.ReadFile(a.Target)
	switch {
	case err == nil && bytes.Equal(old, data):
	case err == nil:
		if err := a.backup(old); err != nil {
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
	return a.Store.SaveState(st)
}

func (a *App) backup(data []byte) error {
	dir := a.Store.BackupsDir()
	name := "config-" + time.Now().Format("20060102-150405.000000")
	if err := store.WriteFile(filepath.Join(dir, name), data); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
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
			return err
		}
		backups = backups[1:]
	}
	return nil
}

// Rebuild is `kx build`: pull namespace switches in and regenerate the target.
// With force the target may be unreadable, so namespaces are best effort then.
func (a *App) Rebuild(force bool) error {
	if err := a.syncNamespaces(); err != nil && !force {
		return err
	}
	return a.Build(force, nil)
}
