package store

import (
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd/api"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"kubernetes-admin@kubernetes":            "kubernetes-admin-kubernetes",
		"arn:aws:eks:eu-west-1:123:cluster/shop": "shop",
		"gke_proj_europe-west1_main":             "gke_proj_europe-west1_main",
		"@@@":                                    "cluster",
		"trailing/":                              "trailing",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	good := func() *api.Config {
		cfg := api.NewConfig()
		cfg.Clusters["c"] = &api.Cluster{Server: "https://k8s.example:6443"}
		cfg.AuthInfos["c"] = &api.AuthInfo{Token: "t"}
		cfg.Contexts["c"] = &api.Context{Cluster: "c", AuthInfo: "c"}
		return cfg
	}
	for _, tc := range []struct {
		name   string
		change func(*api.Config)
		want   string // substring of the error, "" for valid
	}{
		{"token", func(*api.Config) {}, ""},
		{"exec", func(c *api.Config) { c.AuthInfos["c"] = &api.AuthInfo{Exec: &api.ExecConfig{Command: "aws"}} }, ""},
		{"client cert", func(c *api.Config) {
			c.AuthInfos["c"] = &api.AuthInfo{ClientCertificateData: []byte("c"), ClientKeyData: []byte("k")}
		}, ""},
		{"no server", func(c *api.Config) { c.Clusters["c"].Server = "" }, "no server"},
		{"not a url", func(c *api.Config) { c.Clusters["c"].Server = "k8s.example:6443" }, "not an http(s) URL"},
		{"no user", func(c *api.Config) { c.Contexts["c"].AuthInfo = "" }, "no user"},
		{"cert without key", func(c *api.Config) { c.AuthInfos["c"] = &api.AuthInfo{ClientCertificateData: []byte("c")} }, "without its key"},
		{"key without cert", func(c *api.Config) { c.AuthInfos["c"] = &api.AuthInfo{ClientKeyData: []byte("k")} }, "without its certificate"},
		{"no credentials", func(c *api.Config) { c.AuthInfos["c"] = &api.AuthInfo{} }, "no credentials"},
	} {
		cfg := good()
		tc.change(cfg)
		err := Validate(cfg, "c")
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}
