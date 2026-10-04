package store

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

// Ref addresses a whole client ("acme") or one of its clusters ("acme/prod").
type Ref struct {
	Client, Cluster string
}

func (r Ref) String() string {
	if r.Cluster == "" {
		return r.Client
	}
	return r.Client + "/" + r.Cluster
}

func ParseRef(s string) (Ref, error) {
	client, cluster, _ := strings.Cut(s, "/")
	r := Ref{Client: client, Cluster: cluster}
	if !ValidName(client) || strings.Contains(s, "/") && !ValidName(cluster) {
		return Ref{}, fmt.Errorf("invalid name %q: want <client> or <client>/<cluster> of [A-Za-z0-9._-]", s)
	}
	return r, nil
}

// Store keeps one self-contained kubeconfig per client/cluster under a directory.
type Store struct {
	dir string
}

func (s *Store) clustersDir() string { return filepath.Join(s.dir, "clusters") }

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.yaml") }

func (s *Store) BackupsDir() string { return filepath.Join(s.dir, "backups") }

func (s *Store) path(r Ref) string {
	return filepath.Join(s.clustersDir(), r.Client, r.Cluster+".yaml")
}

// Clusters lists every stored cluster, sorted by client then cluster.
func (s *Store) Clusters() ([]Ref, error) {
	var refs []Ref
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
				refs = append(refs, Ref{Client: c.Name(), Cluster: name})
			}
		}
	}
	return refs, nil
}

// Expand resolves refs to concrete clusters; a client ref yields all its clusters.
func (s *Store) Expand(args []string) ([]Ref, error) {
	all, err := s.Clusters()
	if err != nil {
		return nil, err
	}
	var out []Ref
	for _, a := range args {
		r, err := ParseRef(a)
		if err != nil {
			return nil, err
		}
		found := false
		for _, c := range all {
			if c.Client == r.Client && (r.Cluster == "" || c.Cluster == r.Cluster) {
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

func (s *Store) Exists(r Ref) bool {
	_, err := os.Stat(s.path(r))
	return err == nil
}

func (s *Store) Get(r Ref) (*api.Config, error) {
	cfg, err := clientcmd.LoadFromFile(s.path(r))
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", r, err)
	}
	return cfg, nil
}

func (s *Store) Put(r Ref, cfg *api.Config) error {
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("encode %s: %w", r, err)
	}
	return WriteFile(s.path(r), data)
}

func (s *Store) Remove(r Ref) error {
	if err := os.Remove(s.path(r)); err != nil {
		return err
	}
	// Drop the client directory once its last cluster is gone; a non-empty
	// directory makes Remove fail, which is exactly what we want.
	_ = os.Remove(filepath.Dir(s.path(r)))
	return nil
}

// Move renames a cluster, rewriting the context/cluster/user names inside.
func (s *Store) Move(from, to Ref) error {
	cfg, err := s.Get(from)
	if err != nil {
		return err
	}
	moved, err := Extract(cfg, from.String(), to.String())
	if err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	if err := s.Put(to, moved); err != nil {
		return err
	}
	return s.Remove(from)
}

type State struct {
	// Disabled holds client and client/cluster refs excluded from the build.
	Disabled []string `json:"disabled,omitempty"`
	// Generated lists the contexts written by the last build; anything else
	// found in the target kubeconfig was put there by someone else.
	Generated []string `json:"generated,omitempty"`
}

func (s *Store) LoadState() (*State, error) {
	st := &State{}
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

func (s *Store) SaveState(st *State) error {
	slices.Sort(st.Disabled)
	st.Disabled = slices.Compact(st.Disabled)
	data, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	return WriteFile(s.statePath(), data)
}

func (st *State) Enabled(r Ref) bool {
	return !slices.Contains(st.Disabled, r.Client) && !slices.Contains(st.Disabled, r.String())
}

// Forget drops every disabled entry that refers to r or, for a client, to its clusters.
func (st *State) Forget(r Ref) {
	st.Disabled = slices.DeleteFunc(st.Disabled, func(d string) bool {
		return d == r.String() || r.Cluster == "" && strings.HasPrefix(d, r.Client+"/")
	})
}

func (st *State) Disable(r Ref) {
	if r.Cluster == "" {
		st.Forget(r)
	} else if slices.Contains(st.Disabled, r.Client) {
		return
	}
	st.Disabled = append(st.Disabled, r.String())
}

// Enable turns r on. Enabling one cluster of a disabled client keeps its
// siblings off, so the client entry is replaced with per-cluster entries.
func (st *State) Enable(r Ref, siblings []Ref) {
	if r.Cluster != "" && slices.Contains(st.Disabled, r.Client) {
		st.Forget(Ref{Client: r.Client})
		for _, s := range siblings {
			if s.Client == r.Client && s != r {
				st.Disabled = append(st.Disabled, s.String())
			}
		}
		return
	}
	st.Forget(r)
}

// Prune drops disabled entries pointing at clients or clusters that no longer exist.
func (st *State) Prune(refs []Ref) {
	st.Disabled = slices.DeleteFunc(st.Disabled, func(d string) bool {
		return !slices.ContainsFunc(refs, func(r Ref) bool { return d == r.Client || d == r.String() })
	})
}

// WriteFile replaces path atomically so Lens/k9s never read a half-written file.
func WriteFile(path string, data []byte) (err error) {
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

func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Dir() string { return s.dir }
