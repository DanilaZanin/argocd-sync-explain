package collect

import (
	"context"
	"encoding/json"
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
			http.Error(w, `{"error":"permission denied"}`, http.StatusForbidden)
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
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("403 must become a permission denied reason: %v", err)
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
	if depth >= 10 {
		t.Fatalf("depth %d is not bounded", depth)
	}
}
