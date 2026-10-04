package store

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// ReadKubeconfig loads a kubeconfig from a file or stdin ("-") with relative
// file references resolved against its location.
func ReadKubeconfig(path string, stdin io.Reader) (*api.Config, error) {
	var (
		cfg    *api.Config
		origin string
		err    error
	)
	if path == "-" {
		data, err := io.ReadAll(io.LimitReader(stdin, 16<<20))
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if cfg, err = clientcmd.Load(data); err != nil {
			return nil, notKubeconfig("stdin", err)
		}
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		origin = filepath.Join(wd, "stdin")
	} else {
		if origin, err = filepath.Abs(path); err != nil {
			return nil, err
		}
		if cfg, err = clientcmd.LoadFromFile(origin); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil, err
			}
			return nil, notKubeconfig(path, err)
		}
	}
	for _, c := range cfg.Clusters {
		c.LocationOfOrigin = origin
	}
	for _, a := range cfg.AuthInfos {
		a.LocationOfOrigin = origin
	}
	if err := clientcmd.ResolveLocalPaths(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Extract returns a self-contained config holding only context ctx, with the
// context, its cluster and its user all renamed to name. Referenced
// certificate files are inlined so the result no longer depends on them.
func Extract(cfg *api.Config, ctx, name string) (*api.Config, error) {
	c, ok := cfg.Contexts[ctx]
	if !ok {
		return nil, fmt.Errorf("context %q not found", ctx)
	}
	cl, ok := cfg.Clusters[c.Cluster]
	if !ok {
		return nil, fmt.Errorf("context %q: cluster %q not found", ctx, c.Cluster)
	}
	out := api.NewConfig()
	out.Clusters[name] = cl.DeepCopy()
	nc := c.DeepCopy()
	nc.Cluster = name
	if c.AuthInfo != "" {
		a, ok := cfg.AuthInfos[c.AuthInfo]
		if !ok {
			return nil, fmt.Errorf("context %q: user %q not found", ctx, c.AuthInfo)
		}
		out.AuthInfos[name] = a.DeepCopy()
		nc.AuthInfo = name
	}
	out.Contexts[name] = nc
	out.CurrentContext = name
	if err := api.FlattenConfig(out); err != nil {
		return nil, fmt.Errorf("context %q: inline certificate files: %w", ctx, err)
	}
	return out, nil
}

// Validate checks that a config made by Extract can work at all: the cluster
// has an http(s) server address and the user carries some credentials.
func Validate(cfg *api.Config, name string) error {
	ctx := cfg.Contexts[name]
	if ctx == nil {
		return fmt.Errorf("context %q not found", name)
	}
	cl := cfg.Clusters[ctx.Cluster]
	if cl == nil || cl.Server == "" {
		return errors.New("no server address")
	}
	u, err := url.Parse(cl.Server)
	if err != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("server %q is not an http(s) URL", cl.Server)
	}
	ai := cfg.AuthInfos[ctx.AuthInfo]
	if ai == nil {
		return errors.New("no user, so no credentials")
	}
	cert, key := len(ai.ClientCertificateData) > 0, len(ai.ClientKeyData) > 0
	switch {
	case cert && !key:
		return errors.New("client certificate without its key")
	case key && !cert:
		return errors.New("client key without its certificate")
	case cert, ai.Token != "", ai.TokenFile != "", ai.Exec != nil, ai.AuthProvider != nil, ai.Username != "":
		return nil
	}
	return errors.New("no credentials: expected a token, a client certificate or an exec plugin")
}

func Merge(cfgs []*api.Config) *api.Config {
	out := api.NewConfig()
	for _, c := range cfgs {
		for k, v := range c.Clusters {
			out.Clusters[k] = v
		}
		for k, v := range c.AuthInfos {
			out.AuthInfos[k] = v
		}
		for k, v := range c.Contexts {
			out.Contexts[k] = v
		}
	}
	return out
}

func ContextNames(cfg *api.Config) []string {
	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func ValidName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !nameRune(r) {
			return false
		}
	}
	return true
}

func nameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
}

// Sanitize derives a cluster name from an arbitrary context name. For EKS
// ARNs ("arn:aws:eks:...:cluster/foo") only the part after the last slash is kept.
func Sanitize(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	b := []rune(s)
	for i, r := range b {
		if !nameRune(r) {
			b[i] = '-'
		}
	}
	out := strings.Trim(string(b), "-.")
	if out == "" {
		return "cluster"
	}
	return out
}

// notKubeconfig turns a decoder error into something a person can act on.
// Go type names in "cannot unmarshal" errors mean nothing to them.
func notKubeconfig(what string, err error) error {
	if strings.Contains(err.Error(), "cannot unmarshal") {
		return fmt.Errorf("%s is not a kubeconfig: expected YAML with clusters, users and contexts", what)
	}
	return fmt.Errorf("%s is not a kubeconfig: %w", what, err)
}
