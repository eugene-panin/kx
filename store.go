package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

// ref addresses a whole client ("acme") or one of its clusters ("acme/prod").
type ref struct {
	client, cluster string
}

func (r ref) String() string {
	if r.cluster == "" {
		return r.client
	}
	return r.client + "/" + r.cluster
}

func parseRef(s string) (ref, error) {
	client, cluster, _ := strings.Cut(s, "/")
	r := ref{client: client, cluster: cluster}
	if !validName(client) || strings.Contains(s, "/") && !validName(cluster) {
		return ref{}, fmt.Errorf("invalid name %q: want <client> or <client>/<cluster> of [A-Za-z0-9._-]", s)
	}
	return r, nil
}

type store struct {
	dir string
}

func (s *store) clustersDir() string { return filepath.Join(s.dir, "clusters") }
func (s *store) statePath() string   { return filepath.Join(s.dir, "state.yaml") }
func (s *store) backupsDir() string  { return filepath.Join(s.dir, "backups") }

func (s *store) path(r ref) string {
	return filepath.Join(s.clustersDir(), r.client, r.cluster+".yaml")
}

// clusters lists every stored cluster, sorted by client then cluster.
func (s *store) clusters() ([]ref, error) {
	var refs []ref
	clients, err := os.ReadDir(s.clustersDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		if !c.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.clustersDir(), c.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			name, ok := strings.CutSuffix(f.Name(), ".yaml")
			if ok && !f.IsDir() {
				refs = append(refs, ref{client: c.Name(), cluster: name})
			}
		}
	}
	return refs, nil
}

// expand resolves refs to concrete clusters; a client ref yields all its clusters.
func (s *store) expand(args []string) ([]ref, error) {
	all, err := s.clusters()
	if err != nil {
		return nil, err
	}
	var out []ref
	for _, a := range args {
		r, err := parseRef(a)
		if err != nil {
			return nil, err
		}
		found := false
		for _, c := range all {
			if c.client == r.client && (r.cluster == "" || c.cluster == r.cluster) {
				found = true
				if !slices.Contains(out, c) {
					out = append(out, c)
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("%s: not found", r)
		}
	}
	return out, nil
}

func (s *store) exists(r ref) bool {
	_, err := os.Stat(s.path(r))
	return err == nil
}

func (s *store) get(r ref) (*api.Config, error) {
	cfg, err := clientcmd.LoadFromFile(s.path(r))
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", r, err)
	}
	return cfg, nil
}

func (s *store) put(r ref, cfg *api.Config) error {
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("encode %s: %w", r, err)
	}
	return writeFile(s.path(r), data)
}

func (s *store) remove(r ref) error {
	if err := os.Remove(s.path(r)); err != nil {
		return err
	}
	// Drop the client directory once its last cluster is gone; a non-empty
	// directory makes Remove fail, which is exactly what we want.
	_ = os.Remove(filepath.Dir(s.path(r)))
	return nil
}

// move renames a cluster, rewriting the context/cluster/user names inside.
func (s *store) move(from, to ref) error {
	cfg, err := s.get(from)
	if err != nil {
		return err
	}
	moved, err := extract(cfg, from.String(), to.String())
	if err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	if err := s.put(to, moved); err != nil {
		return err
	}
	return s.remove(from)
}

type state struct {
	// Disabled holds client and client/cluster refs excluded from the build.
	Disabled []string `json:"disabled,omitempty"`
	// Generated lists the contexts written by the last build; anything else
	// found in the target kubeconfig was put there by someone else.
	Generated []string `json:"generated,omitempty"`
}

func (s *store) loadState() (*state, error) {
	st := &state{}
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.statePath(), err)
	}
	return st, nil
}

func (s *store) saveState(st *state) error {
	slices.Sort(st.Disabled)
	st.Disabled = slices.Compact(st.Disabled)
	data, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	return writeFile(s.statePath(), data)
}

func (st *state) enabled(r ref) bool {
	return !slices.Contains(st.Disabled, r.client) && !slices.Contains(st.Disabled, r.String())
}

// forget drops every disabled entry that refers to r or, for a client, to its clusters.
func (st *state) forget(r ref) {
	st.Disabled = slices.DeleteFunc(st.Disabled, func(d string) bool {
		return d == r.String() || r.cluster == "" && strings.HasPrefix(d, r.client+"/")
	})
}

func (st *state) disable(r ref) {
	if r.cluster == "" {
		st.forget(r)
	} else if slices.Contains(st.Disabled, r.client) {
		return
	}
	st.Disabled = append(st.Disabled, r.String())
}

// enable turns r on. Enabling one cluster of a disabled client keeps its
// siblings off, so the client entry is replaced with per-cluster entries.
func (st *state) enable(r ref, siblings []ref) {
	if r.cluster != "" && slices.Contains(st.Disabled, r.client) {
		st.forget(ref{client: r.client})
		for _, s := range siblings {
			if s.client == r.client && s != r {
				st.Disabled = append(st.Disabled, s.String())
			}
		}
		return
	}
	st.forget(r)
}

// prune drops disabled entries pointing at clients or clusters that no longer exist.
func (st *state) prune(refs []ref) {
	st.Disabled = slices.DeleteFunc(st.Disabled, func(d string) bool {
		return !slices.ContainsFunc(refs, func(r ref) bool { return d == r.client || d == r.String() })
	})
}

// writeFile replaces path atomically so Lens/k9s never read a half-written file.
func writeFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".kx-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
