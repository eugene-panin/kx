// Package kxtest holds fixtures shared by kx tests: an isolated KX_HOME and
// target kubeconfig, a kubeadm-style kubeconfig, and a fake API server.
package kxtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// Kubeadm is a kubeconfig as kubeadm writes it: every cluster built this way
// has the same cluster/user/context names, which is what kx exists to untangle.
// %s is the server URL.
const Kubeadm = `apiVersion: v1
kind: Config
clusters:
- name: kubernetes
  cluster:
    server: %s
    certificate-authority: ca.crt
users:
- name: kubernetes-admin
  user:
    token: secret
contexts:
- name: kubernetes-admin@kubernetes
  context:
    cluster: kubernetes
    user: kubernetes-admin
current-context: kubernetes-admin@kubernetes
`

// Env points KX_HOME and KX_KUBECONFIG at a temp dir. Src holds input
// kubeconfigs and a ca.crt they refer to.
type Env struct {
	t      *testing.T
	Home   string
	Target string
	Src    string
}

func NewEnv(t *testing.T) *Env {
	t.Helper()
	dir := t.TempDir()
	e := &Env{t: t, Home: filepath.Join(dir, "kx"), Target: filepath.Join(dir, "kube", "config"), Src: filepath.Join(dir, "src")}
	t.Setenv("KX_HOME", e.Home)
	t.Setenv("KX_KUBECONFIG", e.Target)
	if err := os.MkdirAll(e.Src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Src, "ca.crt"), []byte("CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// Kubeconfig writes the Kubeadm fixture for server into Src and returns its path.
func (e *Env) Kubeconfig(name, server string) string {
	e.t.Helper()
	p := filepath.Join(e.Src, name)
	if err := os.WriteFile(p, []byte(strings.Replace(Kubeadm, "%s", server, 1)), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// Contexts lists the contexts in the target kubeconfig.
func (e *Env) Contexts() []string {
	e.t.Helper()
	cfg, err := clientcmd.LoadFromFile(e.Target)
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (e *Env) WantContexts(want ...string) {
	e.t.Helper()
	if got := e.Contexts(); !slices.Equal(got, want) {
		e.t.Errorf("contexts = %v, want %v", got, want)
	}
}

type APIOpts struct {
	NoSSR   bool     // pre-1.28: no SelfSubjectReview
	Hang    bool     // /version never answers
	Version string   // default v1.31.2
	Failing []string // readyz checks reported as failed
	Nodes   []string // node Status column; nil answers 403
}

// FakeAPI is a TLS API server that accepts the token from the kubeadm fixture.
// All httptest TLS servers share one certificate, so one CA file trusts them all.
func FakeAPI(t *testing.T, opts APIOpts) *httptest.Server {
	t.Helper()
	if opts.Version == "" {
		opts.Version = "v1.31.2"
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
		if opts.Hang {
			<-r.Context().Done()
			return
		}
		fmt.Fprintf(w, `{"gitVersion":%q}`, opts.Version)
	})
	mux.HandleFunc("POST /apis/authentication.k8s.io/v1/selfsubjectreviews", func(w http.ResponseWriter, r *http.Request) {
		if opts.NoSSR {
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
		if len(opts.Failing) == 0 {
			w.Write([]byte("[+]ping ok\n[+]etcd ok\nreadyz check passed\n"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "[+]ping ok\n")
		for _, f := range opts.Failing {
			fmt.Fprintf(w, "[-]%s failed: reason withheld\n", f)
		}
		fmt.Fprint(w, "readyz check failed\n")
	})
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		if opts.Nodes == nil || !strings.Contains(r.Header.Get("Accept"), "as=Table") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"kind":"Table","columnDefinitions":[{"name":"Name"},{"name":"Status"},{"name":"Roles"}],"rows":[`)
		for i, st := range opts.Nodes {
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

// WriteCA makes the fixtures' ca.crt trust the httptest servers.
func (e *Env) WriteCA(srv *httptest.Server) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.Src, "ca.crt"), CertPEM(srv.Certificate().Raw), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func CertPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func SelfSigned(t *testing.T, notAfter time.Time) *x509.Certificate {
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
