package main

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

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

const keepBackups = 10

type app struct {
	store  *store
	target string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (a *app) loadTarget() (*api.Config, error) {
	cfg, err := clientcmd.LoadFromFile(a.target)
	if errors.Is(err, fs.ErrNotExist) {
		return api.NewConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", a.target, err)
	}
	return cfg, nil
}

// unmanaged returns contexts in the target kubeconfig that kx neither stores
// nor generated last time: a rebuild would silently drop them.
func (a *app) unmanaged() ([]string, error) {
	cur, err := a.loadTarget()
	if err != nil {
		return nil, err
	}
	refs, err := a.store.clusters()
	if err != nil {
		return nil, err
	}
	st, err := a.store.loadState()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range contextNames(cur) {
		known := slices.Contains(st.Generated, name) ||
			slices.ContainsFunc(refs, func(r ref) bool { return r.String() == name })
		if !known {
			out = append(out, name)
		}
	}
	return out, nil
}

// checkTarget refuses to proceed while the target holds contexts kx would lose.
func (a *app) checkTarget() error {
	names, err := a.unmanaged()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	return fmt.Errorf("%s has contexts not managed by kx:\n  %s\n"+
		"take them over with `kx import-current -c <client>` or drop them with `kx build --force`",
		a.target, strings.Join(names, "\n  "))
}

// prepare runs before any change: it refuses to touch a target holding foreign
// contexts and pulls namespace switches made by other tools into the store.
func (a *app) prepare() error {
	if err := a.checkTarget(); err != nil {
		return err
	}
	return a.syncNamespaces()
}

// syncNamespaces copies namespaces switched in the target (k9s, kubens,
// kubectl config set-context) back into the store, so a rebuild keeps them
// and they survive off/on.
func (a *app) syncNamespaces() error {
	cur, err := a.loadTarget()
	if err != nil {
		return err
	}
	refs, err := a.store.clusters()
	if err != nil {
		return err
	}
	for _, r := range refs {
		tc := cur.Contexts[r.String()]
		if tc == nil {
			continue
		}
		cfg, err := a.store.get(r)
		if err != nil {
			return err
		}
		sc := cfg.Contexts[r.String()]
		if sc == nil || sc.Namespace == tc.Namespace {
			continue
		}
		sc.Namespace = tc.Namespace
		if err := a.store.put(r, cfg); err != nil {
			return err
		}
	}
	return nil
}

// build regenerates the target. renames maps old context names to new ones so
// that a renamed current-context survives.
func (a *app) build(force bool, renames map[string]string) error {
	if !force {
		if err := a.checkTarget(); err != nil {
			return err
		}
	}
	refs, err := a.store.clusters()
	if err != nil {
		return err
	}
	st, err := a.store.loadState()
	if err != nil {
		return err
	}
	var (
		parts []*api.Config
		names []string
	)
	for _, r := range refs {
		if !st.enabled(r) {
			continue
		}
		cfg, err := a.store.get(r)
		if err != nil {
			return err
		}
		parts = append(parts, cfg)
		names = append(names, contextNames(cfg)...)
	}
	out := merge(parts)
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
	old, err := os.ReadFile(a.target)
	switch {
	case err == nil && bytes.Equal(old, data):
	case err == nil:
		if err := a.backup(old); err != nil {
			return fmt.Errorf("backup %s: %w", a.target, err)
		}
		fallthrough
	case errors.Is(err, fs.ErrNotExist):
		if err := writeFile(a.target, data); err != nil {
			return err
		}
	default:
		return err
	}
	st.Generated = names
	return a.store.saveState(st)
}

func (a *app) backup(data []byte) error {
	dir := a.store.backupsDir()
	name := "config-" + time.Now().Format("20060102-150405.000000")
	if err := writeFile(filepath.Join(dir, name), data); err != nil {
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
