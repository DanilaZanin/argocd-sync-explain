package collect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveConnPrecedence(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	body := `contexts:
- name: argocd.example.com
  server: argocd.example.com
  user: argocd.example.com
current-context: argocd.example.com
servers:
- server: argocd.example.com
  insecure: true
users:
- name: argocd.example.com
  auth-token: file-token
`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// File only.
	c, err := ResolveConn("", "", cfg, "", false, false, env(nil))
	if err != nil || c.Server != "argocd.example.com" || c.Token != "file-token" || !c.Insecure {
		t.Fatalf("%+v %v", c, err)
	}
	// Environment beats the file; flags beat the environment.
	c, _ = ResolveConn("", "", cfg, "", false, false, env(map[string]string{"ARGOCD_SERVER": "env:443", "ARGOCD_AUTH_TOKEN": "env-token"}))
	if c.Server != "env:443" || c.Token != "env-token" {
		t.Fatalf("%+v", c)
	}
	c, _ = ResolveConn("flag:443", "flag-token", cfg, "", false, false, env(map[string]string{"ARGOCD_SERVER": "env:443", "ARGOCD_AUTH_TOKEN": "env-token"}))
	if c.Server != "flag:443" || c.Token != "flag-token" {
		t.Fatalf("%+v", c)
	}
	// No config, nothing set: empty, no error (the tool falls back to kubeconfig mode).
	c, err = ResolveConn("", "", filepath.Join(t.TempDir(), "none"), "", false, false, env(nil))
	if err != nil || c.Server != "" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestAPIClientMapsErrorsToUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/applications/denied"):
			http.Error(w, `{"error":"denied or does not exist"}`, http.StatusForbidden)
		case r.URL.Path == "/api/v1/applications/ok":
			if r.URL.Query().Get("appNamespace") != "argocd" || r.Method != http.MethodGet {
				t.Errorf("unexpected request %s %s", r.Method, r.URL)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "ok"}, "spec": map[string]any{"project": "default"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewAPIClient(strings.TrimPrefix(srv.URL, "http://"), "tok", true, false)
	a, err := c.Application(context.Background(), "argocd", "ok")
	if err != nil || a.Metadata.Name != "ok" || a.Metadata.Namespace != "argocd" {
		t.Fatalf("%+v %v", a, err)
	}
	_, err = c.Application(context.Background(), "argocd", "denied")
	if err == nil || !strings.Contains(err.Error(), "denied or does not exist") {
		t.Fatalf("403 must become a denied or does not exist reason: %v", err)
	}
	if _, err := c.Tree(context.Background(), "argocd", "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
	bad := NewAPIClient(strings.TrimPrefix(srv.URL, "http://"), "wrong", true, false)
	if _, err := bad.Version(context.Background()); err == nil {
		t.Fatal("a bad token must fail")
	}
}

// fakeSource serves applications from a map.
type fakeSource struct{ apps map[string]*model.Application }

func (f *fakeSource) Mode() string                            { return "fake" }
func (f *fakeSource) Version(context.Context) (string, error) { return "v3.5.3", nil }
func (f *fakeSource) Project(context.Context, string) (*model.AppProject, error) {
	return nil, &ErrUnavailable{Reason: "no project in the fake"}
}
func (f *fakeSource) Tree(context.Context, string, string) (*model.ResourceTree, error) {
	return &model.ResourceTree{}, nil
}
func (f *fakeSource) Events(context.Context, string, string) ([]model.Event, error) { return nil, nil }
func (f *fakeSource) Application(_ context.Context, ns, name string) (*model.Application, error) {
	if a, ok := f.apps[ns+"/"+name]; ok {
		return a, nil
	}
	return nil, &ErrUnavailable{Reason: "application: permission denied (HTTP 403)"}
}

func appWithChildren(name string, children ...string) *model.Application {
	a := &model.Application{}
	a.Metadata.Name, a.Metadata.Namespace = name, "argocd"
	a.Spec.Destination.Server = "https://kubernetes.default.svc"
	for _, c := range children {
		a.Status.Resources = append(a.Status.Resources, model.ResourceStatus{Group: "argoproj.io", Kind: "Application", Namespace: "argocd", Name: c})
	}
	return a
}

func TestCollectChildrenCyclesAndUnreadableChildren(t *testing.T) {
	src := &fakeSource{apps: map[string]*model.Application{
		"argocd/root": appWithChildren("root", "a", "hidden"),
		"argocd/a":    appWithChildren("a", "root"), // cycle back to the root
	}}
	snap, err := Collect(context.Background(), src, "argocd", "root", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Children["argocd/a"]; !ok {
		t.Fatalf("child a missing: %+v", snap.Children)
	}
	if len(snap.Children["argocd/a"].Children) != 0 {
		t.Fatal("the cycle back to the root must not be followed")
	}
	if why := snap.Unavailable["child:argocd/hidden"]; !strings.Contains(why, "permission denied") {
		t.Fatalf("unreadable child must be recorded with its reason: %+v", snap.Unavailable)
	}
	if why := snap.Children["argocd/a"].Unavailable["child:argocd/root"]; !strings.Contains(why, "cycle") {
		t.Fatalf("the cycle must be recorded: %+v", snap.Children["argocd/a"].Unavailable)
	}
	if snap.Unavailable["project"] == "" {
		t.Fatal("unavailable project must be recorded")
	}
	if _, err := Collect(context.Background(), src, "argocd", "nope", time.Now()); err == nil {
		t.Fatal("an unreadable root application is a tool error")
	}
}

func TestChainDepthIsBounded(t *testing.T) {
	src := &fakeSource{apps: map[string]*model.Application{}}
	for i := 0; i < 20; i++ {
		name := "n" + string(rune('a'+i))
		next := "n" + string(rune('a'+i+1))
		src.apps["argocd/"+name] = appWithChildren(name, next)
	}
	snap, err := Collect(context.Background(), src, "argocd", "na", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	depth := 0
	for cur := snap; len(cur.Children) > 0; {
		for _, c := range cur.Children {
			cur = c
		}
		depth++
	}
	if depth != MaxDepth-1 {
		t.Fatalf("depth %d, want %d", depth, MaxDepth-1)
	}
	cur := snap
	for i := 0; i < MaxDepth-1; i++ {
		for _, c := range cur.Children {
			cur = c
		}
	}
	found := false
	for k, why := range cur.Unavailable {
		if strings.HasPrefix(k, "child:") && strings.Contains(why, "depth limit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the cut must be recorded, not silent: %+v", cur.Unavailable)
	}
}

func TestChildrenOfAnotherClusterAreNotReadFromHere(t *testing.T) {
	src := &fakeSource{apps: map[string]*model.Application{
		"argocd/root":  appWithChildren("root", "child"),
		"argocd/child": appWithChildren("child"), // a same-named local Application that is NOT the parent's child
	}}
	src.apps["argocd/root"].Spec.Destination.Server = "https://other.example"
	snap, err := Collect(context.Background(), src, "argocd", "root", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Children) != 0 || !strings.Contains(snap.Unavailable["child:argocd/child"], "not the cluster Argo CD runs in") {
		t.Fatalf("%+v %+v", snap.Children, snap.Unavailable)
	}
}

func TestSharedChildIsReadOnce(t *testing.T) {
	src := &fakeSource{apps: map[string]*model.Application{
		"argocd/root": appWithChildren("root", "a", "b"),
		"argocd/a":    appWithChildren("a", "leaf"),
		"argocd/b":    appWithChildren("b", "leaf"),
		"argocd/leaf": appWithChildren("leaf"),
	}}
	snap, err := Collect(context.Background(), src, "argocd", "root", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Children["argocd/a"].Children["argocd/leaf"] != snap.Children["argocd/b"].Children["argocd/leaf"] {
		t.Fatal("a snapshot that was already read is reused")
	}
}

func TestStatusCodesAndSecretRedaction(t *testing.T) {
	const tok = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9.c2lnbmF0dXJl"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/401"):
			http.Error(w, "x", http.StatusUnauthorized)
		case strings.Contains(r.URL.Path, "/403"):
			http.Error(w, "x", http.StatusForbidden)
		default:
			// A misbehaving proxy echoing the request: token, a second JWT and an Authorization header.
			http.Error(w, "upstream failed for Authorization: Bearer "+r.Header.Get("Authorization")[7:]+" other eyJhbGciOiJIUzI1NiJ9.eyJ4IjoieSJ9.c2ln end", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := NewAPIClient(strings.TrimPrefix(srv.URL, "http://"), tok, true, false)
	_, err := c.Application(context.Background(), "argocd", "401")
	var u *ErrUnavailable
	if !errors.As(err, &u) || u.Denied || !strings.Contains(u.Reason, "authentication failed") || strings.Contains(u.Reason, "RBAC") {
		t.Fatalf("401 is authentication, not RBAC: %v", err)
	}
	_, err = c.Application(context.Background(), "argocd", "403")
	if !errors.As(err, &u) || !u.Denied || !strings.Contains(u.Reason, "denied or does not exist") {
		t.Fatalf("403 is a denial: %v", err)
	}
	_, err = c.Application(context.Background(), "argocd", "boom")
	if err == nil {
		t.Fatal("500 must fail")
	}
	for _, canary := range []string{tok, "eyJhbGciOiJIUzI1NiJ9", "c2ln"} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("token material leaked into the error: %s", err)
		}
	}
	if !strings.Contains(Redact("Bearer abcdefghijklmnop1234"), "[redacted]") {
		t.Fatal("Bearer values are redacted")
	}
}

func TestConfigTokenIsNotSentToAnotherServer(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	body := "contexts:\n- {name: prod, server: argocd.example.com, user: prod}\ncurrent-context: prod\nservers:\n- {server: argocd.example.com, insecure: true}\nusers:\n- {name: prod, auth-token: CANARY-TOKEN-123456}\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sawAuth = r.Header.Get("Authorization") }))
	defer srv.Close()
	other := strings.TrimPrefix(srv.URL, "http://")
	c, err := ResolveConn(other, "", cfg, "", false, true, env(nil))
	if err != nil || c.Token != "" || c.Insecure {
		t.Fatalf("an explicit different server gets no token and no TLS settings from the config: %+v %v", c, err)
	}
	// Even if a client were built from it, nothing is sent.
	_, _ = NewAPIClient(c.Server, c.Token, true, false).Version(context.Background())
	if sawAuth != "" {
		t.Fatalf("the listener saw %q", sawAuth)
	}
	// The same server as the context keeps working.
	c, _ = ResolveConn("argocd.example.com", "", cfg, "", false, false, env(nil))
	if c.Token != "CANARY-TOKEN-123456" {
		t.Fatalf("%+v", c)
	}
	// The server from the environment counts as explicit too.
	c, _ = ResolveConn("", "", cfg, "", false, false, env(map[string]string{"ARGOCD_SERVER": other}))
	if c.Token != "" {
		t.Fatalf("%+v", c)
	}
}
