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
	"fmt"
	"io"
	"log"
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

type apiOpts struct {
	noSSR   bool     // pre-1.28: no SelfSubjectReview
	hang    bool     // /version never answers
	version string   // default v1.31.2
	failing []string // readyz checks reported as failed
	nodes   []string // node Status column; nil answers 403
}

// fakeAPI is a TLS API server that accepts the token from the kubeadm fixture.
// All httptest TLS servers share one certificate, so one CA file trusts them all.
func fakeAPI(t *testing.T, opts apiOpts) *httptest.Server {
	t.Helper()
	if opts.version == "" {
		opts.version = "v1.31.2"
	}
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
		if opts.hang {
			<-r.Context().Done()
			return
		}
		fmt.Fprintf(w, `{"gitVersion":%q}`, opts.version)
	})
	mux.HandleFunc("POST /apis/authentication.k8s.io/v1/selfsubjectreviews", func(w http.ResponseWriter, r *http.Request) {
		if opts.noSSR {
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
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if len(opts.failing) == 0 {
			w.Write([]byte("[+]ping ok\n[+]etcd ok\nreadyz check passed\n"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "[+]ping ok\n")
		for _, f := range opts.failing {
			fmt.Fprintf(w, "[-]%s failed: reason withheld\n", f)
		}
		fmt.Fprint(w, "readyz check failed\n")
	})
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		if opts.nodes == nil || !strings.Contains(r.Header.Get("Accept"), "as=Table") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"kind":"Table","columnDefinitions":[{"name":"Name"},{"name":"Status"},{"name":"Roles"}],"rows":[`)
		for i, st := range opts.nodes {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"cells":["node-%d",%q,"<none>"]}`, i, st)
		}
		fmt.Fprint(w, "]}")
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// writeCA makes the fixtures' ca.crt trust the httptest servers.
func writeCA(t *testing.T, e *env, srv *httptest.Server) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.src, "ca.crt"), certPEM(srv.Certificate().Raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCheck(t *testing.T) {
	e := newEnv(t)
	ok := fakeAPI(t, apiOpts{})
	old := fakeAPI(t, apiOpts{noSSR: true})
	slow := fakeAPI(t, apiOpts{hang: true})
	gone := fakeAPI(t, apiOpts{})
	gone.Close()

	writeCA(t, e, ok)
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
	statuses := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && strings.HasPrefix(f[0], "acme/") {
			statuses[f[0]] = f[1]
		}
	}
	if len(statuses) != 2 || statuses["acme/old"] != "ok" || statuses["acme/ok"] != "ok" {
		t.Errorf("explicit check:\n%s", out)
	}
}

func TestCheckUnknownCA(t *testing.T) {
	srv := fakeAPI(t, apiOpts{})
	cfg := api.NewConfig()
	cfg.Clusters["c"] = &api.Cluster{Server: srv.URL, CertificateAuthorityData: certPEM(selfSigned(t, time.Now().Add(time.Hour)).Raw)}
	cfg.AuthInfos["c"] = &api.AuthInfo{Token: "secret"}
	cfg.Contexts["c"] = &api.Context{Cluster: "c", AuthInfo: "c"}
	if res := probe(t.Context(), cfg, "c"); res.Status != "tls" {
		t.Errorf("unknown CA: %+v", res)
	}
}

func TestCheckHealthNodesVersions(t *testing.T) {
	e := newEnv(t)
	fresh := fakeAPI(t, apiOpts{version: "v1.36.2", nodes: []string{"Ready", "Ready,SchedulingDisabled", "NotReady"}})
	stale := fakeAPI(t, apiOpts{version: "v1.33.4-eks-1a2b", failing: []string{"etcd"}})
	writeCA(t, e, fresh)
	e.ok("add", e.kubeconfig("a.yaml", fresh.URL), "-c", "acme", "-n", "fresh")
	e.ok("add", e.kubeconfig("b.yaml", stale.URL), "-c", "acme", "-n", "stale")

	out, err := e.run("", "check", "--json")
	if _, isExit := err.(exitError); !isExit {
		t.Fatalf("a degraded cluster must fail check: err = %v\n%s", err, out)
	}
	var results []checkResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]checkResult{}
	for _, r := range results {
		got[r.Context] = r
	}
	if r := got["acme/fresh"]; r.Health != "ok" || r.Nodes == nil || *r.Nodes != (nodeCount{Ready: 2, Total: 3}) {
		t.Errorf("fresh: %+v nodes=%v", r, r.Nodes)
	}
	if r := got["acme/stale"]; r.Health != "degraded" || len(r.Failing) != 1 || r.Failing[0] != "etcd" || r.Nodes != nil {
		t.Errorf("stale: %+v", r)
	}

	out, _ = e.run("", "check")
	for _, want := range []string{"2/3", "degraded", "v1.33.4-eks-1a2b!", "readyz failing: etcd", "v1.33 is out of upstream support (newest seen: v1.36)"} {
		if !strings.Contains(out, want) {
			t.Errorf("check output lacks %q:\n%s", want, out)
		}
	}

	// Results are remembered for ls.
	var rows []listRow
	if err := json.Unmarshal([]byte(e.ok("ls", "--json")), &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].Version != "v1.36.2" || rows[1].Version != "v1.33.4-eks-1a2b" {
		t.Errorf("ls versions: %+v", rows)
	}
	if out := e.ok("ls"); !strings.Contains(out, "v1.36.2") {
		t.Errorf("ls table lacks the version:\n%s", out)
	}
}

func TestVersionPolicy(t *testing.T) {
	p := newVersionPolicy(map[string]checkResult{
		"a": {Version: "v1.36.2"}, "b": {Version: "v1.34.0+k0s"}, "c": {Version: "garbage"}, "d": {},
	})
	for v, want := range map[string]bool{
		"v1.36.2": false, "v1.35.9": false, "v1.34.1-eks-1a2b": false, "v1.33.4": true, "v1.20.0": true, "": false, "garbage": false,
	} {
		if got := p.outdated(v); got != want {
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
