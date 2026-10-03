//go:build e2e

// Package e2e runs the tool against real Argo CD installations on kind. Every
// scenario is produced for real (git commit, Application, controller reaction)
// and the assertions are made on the JSON output: blocker identity and
// classification, not message text.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/DanilaZanin/argocd-sync-explain/internal/collect"
	"github.com/DanilaZanin/argocd-sync-explain/internal/explain"
	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

const (
	repoURL   = "http://gitea.git.svc.cluster.local:3000/ase/fixtures.git"
	argoNS    = "argocd"
	gitUser   = "ase"
	giteaBase = "ase/fixtures"
)

// Env is the shared environment of one run (one cluster, one Argo CD version).
type Env struct {
	Version string
	Context string
	Bin     string
	Full    bool
	cfg     *rest.Config
	argoAdr string
	token   string
	api     *collect.APIClient
	gitAdr  string
	gitPass string
	stops   []func()
}

var env *Env

func TestMain(m *testing.M) {
	ver := os.Getenv("ASE_E2E_VERSION")
	if ver == "" {
		fmt.Fprintln(os.Stderr, "ASE_E2E_VERSION is required (use make e2e or make e2e-full)")
		os.Exit(2)
	}
	e := &Env{Version: ver, Context: "kind-ase-" + strings.ReplaceAll(ver, ".", "-"),
		Bin: os.Getenv("ASE_BIN"), Full: os.Getenv("ASE_E2E_SET") == "full"}
	if e.Bin == "" {
		fmt.Fprintln(os.Stderr, "ASE_BIN is required")
		os.Exit(2)
	}
	if err := e.setup(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup failed:", err)
		e.close()
		os.Exit(1)
	}
	env = e
	code := m.Run()
	e.close()
	os.Exit(code)
}

func (e *Env) close() {
	for _, s := range e.stops {
		s()
	}
}

func (e *Env) kubectl(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", e.Context}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func (e *Env) setup() error {
	rc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: e.Context})
	cfg, err := rc.ClientConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig context %s: %w", e.Context, err)
	}
	e.cfg = cfg
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Argo CD API through the same port-forward code path the tool uses.
	adr, stop, err := collect.PortForward(ctx, cfg, argoNS)
	if err != nil {
		return err
	}
	e.stops = append(e.stops, stop)
	e.argoAdr = adr

	pw, err := e.kubectl("", "-n", argoNS, "get", "secret", "argocd-initial-admin-secret", "-o", "jsonpath={.data.password}")
	if err != nil {
		return err
	}
	pwb, err := base64.StdEncoding.DecodeString(pw)
	if err != nil {
		return err
	}
	e.token, err = e.login("admin", string(pwb))
	if err != nil {
		return err
	}
	e.api = collect.NewAPIClient(e.argoAdr, e.token, false, true)

	// Gitea: random admin password per run (test value for the throwaway cluster).
	e.gitPass = fmt.Sprintf("T%d-ase", time.Now().UnixNano())
	create := fmt.Sprintf("gitea admin user create --username %s --password '%s' --email ase@example.test --admin --must-change-password=false", gitUser, e.gitPass)
	if out, err := e.kubectl("", "-n", "git", "exec", "deploy/gitea", "--", "su", "git", "-c", create); err != nil {
		if !strings.Contains(out+err.Error(), "already exists") {
			return fmt.Errorf("gitea user: %w", err)
		}
		change := fmt.Sprintf("gitea admin user change-password --username %s --password '%s' --must-change-password=false", gitUser, e.gitPass)
		if _, err := e.kubectl("", "-n", "git", "exec", "deploy/gitea", "--", "su", "git", "-c", change); err != nil {
			return fmt.Errorf("gitea password reset: %w", err)
		}
	}
	gadr, gstop, err := collect.PortForwardPod(ctx, cfg, "git", "app=gitea", 3000)
	if err != nil {
		return err
	}
	e.stops = append(e.stops, gstop)
	e.gitAdr = gadr
	if _, code, _ := e.gitReq("GET", "/api/v1/repos/"+giteaBase, nil); code != 200 {
		body := `{"name":"fixtures","private":false,"auto_init":true,"default_branch":"main"}`
		if b, code, err := e.gitReq("POST", "/api/v1/user/repos", []byte(body)); err != nil || code != 201 {
			return fmt.Errorf("create repo: %v %d %s", err, code, b)
		}
	}
	return nil
}

func (e *Env) login(user, pass string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	var tok string
	err := retry(30*time.Second, func() error {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: insecureTLS()}, Timeout: 10 * time.Second}
		resp, err := c.Post("https://"+e.argoAdr+"/api/v1/session", "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return fmt.Errorf("login HTTP %d: %s", resp.StatusCode, b)
		}
		var r struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		tok = r.Token
		return nil
	})
	return tok, err
}

func retry(d time.Duration, f func() error) error {
	deadline := time.Now().Add(d)
	for {
		err := f()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Second)
	}
}

// ---- git ----

func (e *Env) gitReq(method, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequest(method, "http://"+e.gitAdr+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.SetBasicAuth(gitUser, e.gitPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

// put creates or updates a file and returns the new commit SHA.
func (e *Env) put(t *testing.T, path, content string) string {
	t.Helper()
	var cur struct {
		SHA string `json:"sha"`
	}
	b, code, err := e.gitReq("GET", "/api/v1/repos/"+giteaBase+"/contents/"+path+"?ref=main", nil)
	if err != nil {
		t.Fatal(err)
	}
	method := "POST"
	req := map[string]any{"branch": "main", "message": "fixture " + path, "content": base64.StdEncoding.EncodeToString([]byte(content))}
	if code == 200 {
		_ = json.Unmarshal(b, &cur)
		method, req["sha"] = "PUT", cur.SHA
	}
	rb, _ := json.Marshal(req)
	b, code, err = e.gitReq(method, "/api/v1/repos/"+giteaBase+"/contents/"+path, rb)
	if err != nil || code/100 != 2 {
		t.Fatalf("git put %s: %v HTTP %d %s", path, err, code, b)
	}
	var r struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	_ = json.Unmarshal(b, &r)
	return r.Commit.SHA
}

func (e *Env) remove(t *testing.T, path string) {
	t.Helper()
	b, code, err := e.gitReq("GET", "/api/v1/repos/"+giteaBase+"/contents/"+path+"?ref=main", nil)
	if err != nil || code != 200 {
		t.Fatalf("git get %s: %v %d", path, err, code)
	}
	var cur struct {
		SHA string `json:"sha"`
	}
	_ = json.Unmarshal(b, &cur)
	rb, _ := json.Marshal(map[string]any{"branch": "main", "message": "remove " + path, "sha": cur.SHA})
	if b, code, err := e.gitReq("DELETE", "/api/v1/repos/"+giteaBase+"/contents/"+path, rb); err != nil || code/100 != 2 {
		t.Fatalf("git delete %s: %v %d %s", path, err, code, b)
	}
}

// ---- Argo CD ----

func (e *Env) app(name string) (*model.Application, error) {
	return e.api.Application(context.Background(), argoNS, name)
}

// waitApp polls the Application through the API until cond holds.
func (e *Env) waitApp(t *testing.T, name, desc string, timeout time.Duration, cond func(*model.Application) bool) *model.Application {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *model.Application
	for time.Now().Before(deadline) {
		a, err := e.app(name)
		if err == nil {
			last = a
			if cond(a) {
				return a
			}
		}
		time.Sleep(time.Second)
	}
	dump, _ := json.MarshalIndent(last, "", " ")
	t.Fatalf("timeout waiting for %s on %s; last state:\n%.3000s", desc, name, dump)
	return nil
}

func (e *Env) refresh(name string) {
	_, _ = e.kubectl("", "-n", argoNS, "annotate", "application", name, "argocd.argoproj.io/refresh=hard", "--overwrite")
}

func (e *Env) apply(t *testing.T, yaml string) {
	t.Helper()
	if out, err := e.kubectl(yaml, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
}

func (e *Env) namespace(t *testing.T, ns string) {
	t.Helper()
	e.apply(t, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", ns))
	t.Cleanup(func() { _, _ = e.kubectl("", "delete", "namespace", ns, "--wait=false", "--ignore-not-found") })
}

func (e *Env) deleteApp(name string) {
	_, _ = e.kubectl("", "-n", argoNS, "patch", "application", name, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
	_, _ = e.kubectl("", "-n", argoNS, "delete", "application", name, "--wait=false", "--ignore-not-found")
}

// explain runs the real binary and returns its parsed JSON report and exit code.
func (e *Env) explain(t *testing.T, args ...string) (*explain.Report, int, string) {
	t.Helper()
	full := append([]string{"-o", "json", "--insecure"}, args...)
	cmd := exec.Command(e.Bin, full...)
	cmd.Env = append(os.Environ(), "ARGOCD_SERVER="+e.argoAdr, "ARGOCD_AUTH_TOKEN="+e.token)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", e.Bin, err)
		}
		code = ee.ExitCode()
	}
	var rep explain.Report
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("explain output is not JSON (exit %d): %v\nstdout: %.2000s\nstderr: %s", code, jerr, out.String(), errb.String())
	}
	return &rep, code, errb.String()
}

// text runs the tool in text mode (used to record real outputs).
func (e *Env) text(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(e.Bin, append([]string{"--insecure"}, args...)...)
	cmd.Env = append(os.Environ(), "ARGOCD_SERVER="+e.argoAdr, "ARGOCD_AUTH_TOKEN="+e.token)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// record stores the text output of a scenario for the README (ASE_E2E_RECORD=dir).
func (e *Env) record(t *testing.T, name string, args ...string) {
	dir := os.Getenv("ASE_E2E_RECORD")
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	out := e.text(t, args...)
	_ = os.WriteFile(fmt.Sprintf("%s/%s-%s.txt", dir, e.Version, name), []byte(out), 0o644)
	// The collected snapshot is kept too: it is real controller output and feeds the golden unit tests.
	if len(args) >= 2 && args[0] == "app" && !strings.Contains(strings.Join(args, " "), "--mode") {
		snap, err := collect.Collect(context.Background(), e.api, argoNS, args[1], time.Now())
		if err == nil {
			if b, err := json.MarshalIndent(snap, "", " "); err == nil {
				_ = os.WriteFile(fmt.Sprintf("%s/%s-%s.snapshot.json", dir, e.Version, name), b, 0o644)
			}
		}
	}
}

func (e *Env) setCM(t *testing.T, patch string) {
	t.Helper()
	if out, err := e.kubectl("", "-n", argoNS, "patch", "configmap", "argocd-cm", "--type=merge", "-p", patch); err != nil {
		t.Fatalf("patch argocd-cm: %v %s", err, out)
	}
}

// accountToken creates an API token for a local account through the admin API.
func (e *Env) accountToken(account string) (string, error) {
	var tok string
	err := retry(60*time.Second, func() error {
		req, _ := http.NewRequest("POST", "https://"+e.argoAdr+"/api/v1/account/"+account+"/token", strings.NewReader(`{"name":"e2e"}`))
		req.Header.Set("Authorization", "Bearer "+e.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: insecureTLS()}, Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return fmt.Errorf("token HTTP %d: %s", resp.StatusCode, b)
		}
		var r struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		tok = r.Token
		return nil
	})
	return tok, err
}

func (e *Env) explainWithToken(t *testing.T, token string, args ...string) (*explain.Report, int) {
	t.Helper()
	cmd := exec.Command(e.Bin, append([]string{"-o", "json", "--insecure"}, args...)...)
	cmd.Env = append(os.Environ(), "ARGOCD_SERVER="+e.argoAdr, "ARGOCD_AUTH_TOKEN="+token)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	var rep explain.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("not JSON (exit %d): %v\n%s\n%s", code, err, out.String(), errb.String())
	}
	return &rep, code
}

// namespaceCleanup deletes a namespace the Application creates itself (CreateNamespace=true).
func (e *Env) namespaceCleanup(t *testing.T, ns string) {
	t.Cleanup(func() { _, _ = e.kubectl("", "delete", "namespace", ns, "--wait=false", "--ignore-not-found") })
}

// jobCreated returns the creationTimestamp of a Job, or the zero time when it does not exist.
func (e *Env) jobCreated(t *testing.T, ns, name string) time.Time {
	t.Helper()
	out, err := e.kubectl("", "-n", ns, "get", "job", name, "--ignore-not-found", "-o", "jsonpath={.metadata.creationTimestamp}")
	if err != nil || strings.TrimSpace(out) == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
