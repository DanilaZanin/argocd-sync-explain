package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanilaZanin/argocd-sync-explain/internal/collect"
)

func runCLI(t *testing.T, env map[string]string, args ...string) (code int, out, errOut string) {
	t.Helper()
	dir := t.TempDir()
	so, _ := os.Create(filepath.Join(dir, "out"))
	se, _ := os.Create(filepath.Join(dir, "err"))
	code = run(args, so, se, func(k string) string { return env[k] })
	_ = so.Close()
	_ = se.Close()
	a, _ := os.ReadFile(filepath.Join(dir, "out"))
	b, _ := os.ReadFile(filepath.Join(dir, "err"))
	return code, string(a), string(b)
}

const canary = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjYW5hcnkifQ.Y2FuYXJ5c2ln"

func TestForwardedKeepsPlaintext(t *testing.T) {
	c := forwarded(collect.Conn{PlainText: true, Token: "from-config"}, "127.0.0.1:1234", "flag-token", func(string) string { return "" })
	if !c.PlainText || c.Server != "127.0.0.1:1234" || c.Token != "flag-token" {
		t.Fatalf("%+v", c)
	}
}

func TestPortForwardNeverSendsTheConfigToken(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sawAuth = r.Header.Get("Authorization") }))
	defer srv.Close()
	// ResolveConn took a token from the argocd config (another server's context).
	c := forwarded(collect.Conn{Token: "CANARY-FROM-CONFIG-9999"}, strings.TrimPrefix(srv.URL, "http://"), "", func(string) string { return "" })
	if c.Token != "" {
		t.Fatalf("config token survived: %+v", c)
	}
	c.PlainText = true
	_, _ = collect.NewAPIClient(c.Server, c.Token, c.PlainText, c.Insecure).Version(context.Background())
	if strings.Contains(sawAuth, "CANARY") {
		t.Fatalf("the listener saw %q", sawAuth)
	}
	c = forwarded(collect.Conn{Token: "CANARY-FROM-CONFIG-9999"}, "x", "", func(k string) string {
		if k == "ARGOCD_AUTH_TOKEN" {
			return "env-token"
		}
		return ""
	})
	if c.Token != "env-token" {
		t.Fatalf("an explicit env token is used: %+v", c)
	}
}

func TestTokenNeverAppearsInAnyOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "backend echo "+r.Header.Get("Authorization"), http.StatusInternalServerError)
	}))
	defer srv.Close()
	for _, format := range []string{"text", "json"} {
		code, out, errOut := runCLI(t, map[string]string{"ARGOCD_AUTH_TOKEN": canary}, "app", "x", "--server", strings.TrimPrefix(srv.URL, "http://"), "--plaintext", "-o", format)
		if code != 1 {
			t.Fatalf("exit %d", code)
		}
		for _, s := range []string{out, errOut} {
			if strings.Contains(s, canary) || strings.Contains(s, "Y2FuYXJ5c2ln") {
				t.Fatalf("token leaked (%s): %q", format, s)
			}
		}
	}
}

func TestUnreadableApplicationIsExit3NotExit1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"permission denied"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	code, _, errOut := runCLI(t, map[string]string{"ARGOCD_AUTH_TOKEN": "tok-123456"}, "app", "x", "--server", strings.TrimPrefix(srv.URL, "http://"), "--plaintext")
	if code != 3 || !strings.Contains(errOut, "denied or does not exist") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// -o json still yields a document for jq.
	code, out, _ := runCLI(t, map[string]string{"ARGOCD_AUTH_TOKEN": "tok-123456"}, "app", "x", "--server", strings.TrimPrefix(srv.URL, "http://"), "--plaintext", "-o", "json")
	if code != 3 || !strings.Contains(out, `"verdict": "unknown"`) || !strings.Contains(out, `"exitCode": 3`) {
		t.Fatalf("exit %d: %s", code, out)
	}
	// 401 is an authentication problem: a tool error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) }))
	defer srv2.Close()
	code, _, errOut = runCLI(t, map[string]string{"ARGOCD_AUTH_TOKEN": "tok-123456"}, "app", "x", "--server", strings.TrimPrefix(srv2.URL, "http://"), "--plaintext")
	if code != 1 || !strings.Contains(errOut, "authentication failed") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestConfigTokenIsNotSentToAnExplicitOtherServer(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	body := "contexts:\n- {name: prod, server: argocd.example.com, user: prod}\ncurrent-context: prod\nusers:\n- {name: prod, auth-token: CANARY-FROM-CONFIG-9999}\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = true }))
	defer srv.Close()
	code, _, errOut := runCLI(t, nil, "app", "x", "--server", strings.TrimPrefix(srv.URL, "http://"), "--plaintext", "--argocd-config", cfg)
	if code != 1 || !strings.Contains(errOut, "needs a token") || seen {
		t.Fatalf("exit %d, request seen=%v: %s", code, seen, errOut)
	}
}
