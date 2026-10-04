package probe

import (
	"encoding/base64"
	"strconv"
	"testing"
	"time"

	"github.com/eugene-panin/kx/internal/kxtest"
	"k8s.io/client-go/tools/clientcmd/api"
)

func TestCheckUnknownCA(t *testing.T) {
	srv := kxtest.FakeAPI(t, kxtest.APIOpts{})
	cfg := api.NewConfig()
	cfg.Clusters["c"] = &api.Cluster{Server: srv.URL, CertificateAuthorityData: kxtest.CertPEM(kxtest.SelfSigned(t, time.Now().Add(time.Hour)).Raw)}
	cfg.AuthInfos["c"] = &api.AuthInfo{Token: "secret"}
	cfg.Contexts["c"] = &api.Context{Cluster: "c", AuthInfo: "c"}
	if res := Probe(t.Context(), cfg, "c"); res.Status != "tls" {
		t.Errorf("unknown CA: %+v", res)
	}
}

func TestVersionPolicy(t *testing.T) {
	p := NewVersionPolicy(map[string]Result{
		"a": {Version: "v1.36.2"}, "b": {Version: "v1.34.0+k0s"}, "c": {Version: "garbage"}, "d": {},
	})
	for v, want := range map[string]bool{
		"v1.36.2": false, "v1.35.9": false, "v1.34.1-eks-1a2b": false, "v1.33.4": true, "v1.20.0": true, "": false, "garbage": false,
	} {
		if got := p.Outdated(v); got != want {
			t.Errorf("outdated(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestCredentialsExpiry(t *testing.T) {
	exp := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x","exp":` + strconv.FormatInt(exp.Unix(), 10) + `}`))
	if got := jwtExpiry("eyJhbGciOiJSUzI1NiJ9." + claims + ".sig"); got == nil || !got.Equal(exp) {
		t.Errorf("jwtExpiry = %v, want %v", got, exp)
	}
	noExp := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))
	for _, tok := range []string{"", "opaque-token", "a.b.c", "x." + noExp + ".y"} {
		if got := jwtExpiry(tok); got != nil {
			t.Errorf("jwtExpiry(%q) = %v, want nil", tok, got)
		}
	}

	cert := kxtest.SelfSigned(t, exp)
	cfg := api.NewConfig()
	cfg.Contexts["c"] = &api.Context{AuthInfo: "u"}
	cfg.AuthInfos["u"] = &api.AuthInfo{ClientCertificateData: kxtest.CertPEM(cert.Raw)}
	if got := credentialsExpiry(cfg, "c"); got == nil || !got.Equal(exp) {
		t.Errorf("cert expiry = %v, want %v", got, exp)
	}

	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-time.Hour), "EXPIRED"},
		{now.Add(10 * 24 * time.Hour), "10d!"},
		{now.Add(100 * 24 * time.Hour), "100d"},
	} {
		if got := FormatExpiry(&tc.t, now); got != tc.want {
			t.Errorf("FormatExpiry(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestShortUser(t *testing.T) {
	for in, want := range map[string]string{
		"system:serviceaccount:devops:eugene-panin": "sa:devops/eugene-panin",
		"kubernetes-admin":                          "kubernetes-admin",
		"system:serviceaccount:broken":              "system:serviceaccount:broken",
	} {
		if got := ShortUser(in); got != want {
			t.Errorf("ShortUser(%q) = %q, want %q", in, got, want)
		}
	}
}
