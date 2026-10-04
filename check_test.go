package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd/api"
)

// fakeAPI is a TLS API server that accepts the token from the kubeadm fixture.
// All httptest TLS servers share one certificate, so one CA file trusts them all.
func fakeAPI(t *testing.T, ssr, hang bool) *httptest.Server {
	t.Helper()
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"kind":"Status","message":"Unauthorized"}`))
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		if hang {
			<-r.Context().Done()
			return
		}
		w.Write([]byte(`{"gitVersion":"v1.31.2"}`))
	})
	mux.HandleFunc("POST /apis/authentication.k8s.io/v1/selfsubjectreviews", func(w http.ResponseWriter, r *http.Request) {
		if !ssr {
			http.NotFound(w, r)
			return
		}
		if authed(w, r) {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"status":{"userInfo":{"username":"kubernetes-admin"}}}`))
		}
	})
	mux.HandleFunc("GET /api", func(w http.ResponseWriter, r *http.Request) {
		if authed(w, r) {
			w.Write([]byte(`{"versions":["v1"]}`))
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCheck(t *testing.T) {
	e := newEnv(t)
	ok := fakeAPI(t, true, false)
	old := fakeAPI(t, false, false)
	slow := fakeAPI(t, true, true)
	gone := fakeAPI(t, true, false)
	gone.Close()

	if err := os.WriteFile(filepath.Join(e.src, "ca.crt"), certPEM(ok.Certificate().Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	e.ok("add", e.kubeconfig("ok.yaml", ok.URL), "-c", "acme", "-n", "ok")
	e.ok("add", e.kubeconfig("old.yaml", old.URL), "-c", "acme", "-n", "old")
	e.ok("add", e.kubeconfig("slow.yaml", slow.URL), "-c", "acme", "-n", "slow")
	e.ok("add", e.kubeconfig("gone.yaml", gone.URL), "-c", "acme", "-n", "gone")
	e.ok("add", e.kubeconfig("bad.yaml", ok.URL), "-c", "acme", "-n", "badtoken")
	cfg, err := e.storeConfig("acme/badtoken")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthInfos["acme/badtoken"].Token = "wrong"
	if err := (&store{dir: e.home}).put(ref{"acme", "badtoken"}, cfg); err != nil {
		t.Fatal(err)
	}
	e.ok("off", "acme/old")

	out, err := e.run("", "check", "--json", "--timeout", "300ms")
	if _, isExit := err.(exitError); !isExit {
		t.Fatalf("check: err = %v\n%s", err, out)
	}
	var results []checkResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]checkResult{}
	for _, r := range results {
		got[r.Context] = r
	}
	if _, checked := got["acme/old"]; checked {
		t.Error("disabled cluster checked without --all")
	}
	if r := got["acme/ok"]; r.Status != "ok" || r.Version != "v1.31.2" || r.User != "kubernetes-admin" {
		t.Errorf("ok cluster: %+v", r)
	}
	if r := got["acme/badtoken"]; r.Status != "unauthorized" {
		t.Errorf("bad token: %+v", r)
	}
	if r := got["acme/gone"]; r.Status != "unreachable" {
		t.Errorf("closed server: %+v", r)
	}
	if r := got["acme/slow"]; r.Status != "unreachable" || r.Error != "timeout" {
		t.Errorf("hanging server: %+v", r)
	}

	// Named explicitly, a disabled cluster is checked; without SelfSubjectReview
	// the discovery fallback still proves the credentials.
	out, err = e.run("", "check", "acme/old", "acme/ok")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "acme/old") || strings.Count(out, " ok ") != 2 {
		t.Errorf("explicit check:\n%s", out)
	}
}

func TestCheckUnknownCA(t *testing.T) {
	srv := fakeAPI(t, true, false)
	cfg := api.NewConfig()
	cfg.Clusters["c"] = &api.Cluster{Server: srv.URL, CertificateAuthorityData: certPEM(selfSigned(t, time.Now().Add(time.Hour)).Raw)}
	cfg.AuthInfos["c"] = &api.AuthInfo{Token: "secret"}
	cfg.Contexts["c"] = &api.Context{Cluster: "c", AuthInfo: "c"}
	if res := probe(t.Context(), cfg, "c"); res.Status != "tls" {
		t.Errorf("unknown CA: %+v", res)
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

	cert := selfSigned(t, exp)
	cfg := api.NewConfig()
	cfg.Contexts["c"] = &api.Context{AuthInfo: "u"}
	cfg.AuthInfos["u"] = &api.AuthInfo{ClientCertificateData: certPEM(cert.Raw)}
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
		if got := formatExpiry(&tc.t, now); got != tc.want {
			t.Errorf("formatExpiry(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestShortUser(t *testing.T) {
	for in, want := range map[string]string{
		"system:serviceaccount:devops:eugene-panin": "sa:devops/eugene-panin",
		"kubernetes-admin":                          "kubernetes-admin",
		"system:serviceaccount:broken":              "system:serviceaccount:broken",
	} {
		if got := shortUser(in); got != want {
			t.Errorf("shortUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func selfSigned(t *testing.T, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kx-test"},
		NotBefore:             notAfter.Add(-24 * time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func (e *env) storeConfig(name string) (*api.Config, error) {
	r, err := parseRef(name)
	if err != nil {
		return nil, err
	}
	return (&store{dir: e.home}).get(r)
}
