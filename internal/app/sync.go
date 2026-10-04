package app

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eugene-panin/kx/internal/store"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// Sync pulls hand edits of the target into the store, so a rebuild doesn't
// undo them. A managed context that was edited replaces the stored one; one
// that was deleted by hand is turned off, not removed. It returns a line per
// change worth telling about; namespace switches (k9s, kubens) are taken
// silently. Contexts kx doesn't manage are left for import-current.
func (a *App) Sync() ([]string, error) {
	if _, err := os.Stat(a.Target); errors.Is(err, fs.ErrNotExist) {
		// The whole file is gone: that's a reset, not a list of deletions.
		return nil, nil
	}
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
	var notes []string
	stateChanged := false
	for _, r := range refs {
		name := r.String()
		if cur.Contexts[name] == nil {
			if st.Enabled(r) && slices.Contains(st.Generated, name) {
				st.Disable(r)
				stateChanged = true
				notes = append(notes, fmt.Sprintf("%s: removed from %s by hand, turned off", name, a.TargetName()))
			}
			continue
		}
		edited, err := store.Extract(cur, name, name)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: edited in %s but unusable (%v), kept the stored one", name, a.TargetName(), err))
			continue
		}
		stored, err := a.Store.Get(r)
		if err != nil {
			return notes, err
		}
		what, err := changes(stored, edited, name)
		if err != nil {
			return notes, err
		}
		if len(what) == 0 {
			continue
		}
		if err := a.Store.Put(r, edited); err != nil {
			return notes, err
		}
		if what := slices.DeleteFunc(what, func(w string) bool { return w == "namespace" }); len(what) > 0 {
			notes = append(notes, fmt.Sprintf("%s: took %s from %s", name, strings.Join(what, " and "), a.TargetName()))
		}
	}
	if stateChanged {
		// Turned-off clusters are out of the target already; this just keeps
		// Generated honest for the next foreign-context check.
		st.Generated = slices.DeleteFunc(st.Generated, func(g string) bool { return cur.Contexts[g] == nil })
		if err := a.Store.SaveState(st); err != nil {
			return notes, err
		}
	}
	return notes, nil
}

// changes names the parts of a single-context config that differ.
func changes(old, cur *api.Config, name string) ([]string, error) {
	oc, nc := old.Contexts[name], cur.Contexts[name]
	if oc == nil || nc == nil {
		return []string{"context"}, nil
	}
	var what []string
	same, err := sameEntry(old.Clusters[oc.Cluster], cur.Clusters[nc.Cluster])
	if err != nil {
		return nil, err
	}
	if !same {
		what = append(what, "cluster")
	}
	if same, err = sameEntry(old.AuthInfos[oc.AuthInfo], cur.AuthInfos[nc.AuthInfo]); err != nil {
		return nil, err
	}
	if !same {
		what = append(what, "credentials")
	}
	if oc.Namespace != nc.Namespace {
		what = append(what, "namespace")
	}
	return what, nil
}

// sameEntry compares two clusters or two users by their serialized form,
// which ignores where they were loaded from and nil-versus-empty details.
func sameEntry[T *api.Cluster | *api.AuthInfo](a, b T) (bool, error) {
	encode := func(v T) ([]byte, error) {
		cfg := api.NewConfig()
		switch e := any(v).(type) {
		case *api.Cluster:
			if e != nil {
				cfg.Clusters["x"] = e
			}
		case *api.AuthInfo:
			if e != nil {
				cfg.AuthInfos["x"] = e
			}
		}
		return clientcmd.Write(*cfg)
	}
	ea, err := encode(a)
	if err != nil {
		return false, err
	}
	eb, err := encode(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ea, eb), nil
}

// TargetName is the target path for messages, with ~ for the home directory.
func (a *App) TargetName() string {
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(a.Target, home+string(filepath.Separator)); ok {
			return "~/" + rest
		}
	}
	return a.Target
}
