//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/DanilaZanin/argocd-sync-explain/internal/explain"
)

// ---- manifests ----

func configMap(name string, wave int, extra string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  annotations:
    argocd.argoproj.io/sync-wave: "%d"
%s
data:
  a: b
`, name, wave, extra)
}

// deployment is Healthy quickly when ready is true, and stays Progressing (readiness probe
// fails) when it is false. deadline > 0 sets progressDeadlineSeconds so that it turns Degraded.
func deployment(name string, wave int, ready bool, deadline int) string {
	probe := `
          readinessProbe: {exec: {command: ["false"]}, periodSeconds: 3}`
	if ready {
		probe = ""
	}
	dl := ""
	if deadline > 0 {
		dl = fmt.Sprintf("\n  progressDeadlineSeconds: %d", deadline)
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  annotations:
    argocd.argoproj.io/sync-wave: "%[2]d"
spec:
  replicas: 1%[4]s
  selector: {matchLabels: {app: %[1]s}}
  template:
    metadata: {labels: {app: %[1]s}}
    spec:
      containers:
        - name: main
          image: busybox:1.37
          imagePullPolicy: IfNotPresent
          command: ["sleep", "3600"]%[3]s
`, name, wave, probe, dl)
}

func job(name, hook, script string, wave int, deletePolicy string) string {
	dp := ""
	if deletePolicy != "" {
		dp = "\n    argocd.argoproj.io/hook-delete-policy: " + deletePolicy
	}
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  annotations:
    argocd.argoproj.io/hook: %s
    argocd.argoproj.io/sync-wave: "%d"%s
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: main
          image: busybox:1.37
          imagePullPolicy: IfNotPresent
          command: ["sh", "-c", %q]
`, name, hook, wave, dp, script)
}

func childApp(name, path, extraSpec string) string {
	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: default
  source: {repoURL: %q, targetRevision: main, path: %s}
  destination: {server: "https://kubernetes.default.svc", namespace: %s}
  syncPolicy:
    automated: {}
    retry: {limit: 0}
%s
`, name, repoURL, path, "ase-"+strings.TrimPrefix(path, "ase-"), extraSpec)
}

// ---- Application ----

type appOpts struct {
	Name, Path, Namespace, Project string
	// SyncPolicy is the YAML under spec (default: automated, no retry).
	SyncPolicy string
	RepoURL    string
}

const autoNoRetry = `  syncPolicy:
    automated: {}
    retry: {limit: 0}
`

func appYAML(o appOpts) string {
	if o.Project == "" {
		o.Project = "default"
	}
	if o.RepoURL == "" {
		o.RepoURL = repoURL
	}
	if o.SyncPolicy == "" {
		o.SyncPolicy = autoNoRetry
	}
	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
  namespace: argocd
spec:
  project: %s
  source: {repoURL: %q, targetRevision: main, path: %s}
  destination: {server: "https://kubernetes.default.svc", namespace: %s}
%s`, o.Name, o.Project, o.RepoURL, o.Path, o.Namespace, o.SyncPolicy)
}

// start writes the files, creates the namespace and the Application, and removes the
// Application when the test ends.
func (e *Env) start(t *testing.T, o appOpts, files map[string]string, createNS bool) {
	t.Helper()
	for p, c := range files {
		e.put(t, o.Path+"/"+p, c)
	}
	if createNS {
		e.namespace(t, o.Namespace)
	}
	e.apply(t, appYAML(o))
	t.Cleanup(func() { e.deleteApp(o.Name) })
}

// ---- assertions ----

func findBlocker(r *explain.Report, role, typ, kind, name string) *explain.Blocker {
	for i := range r.Blockers {
		b := &r.Blockers[i]
		if (role == "" || b.Role == role) && (typ == "" || b.Type == typ) && (kind == "" || b.Ref.Kind == kind) && (name == "" || b.Ref.Name == name) {
			return b
		}
	}
	return nil
}

func mustBlocker(t *testing.T, r *explain.Report, role, typ, kind, name string) *explain.Blocker {
	t.Helper()
	b := findBlocker(r, role, typ, kind, name)
	if b == nil {
		dump, _ := json.MarshalIndent(r.Blockers, "", " ")
		t.Fatalf("no blocker role=%q type=%q kind=%q name=%q; blockers:\n%s", role, typ, kind, name, dump)
	}
	return b
}

func expectVerdict(t *testing.T, r *explain.Report, code int, verdict string, exit int) {
	t.Helper()
	if r.Verdict != verdict || code != exit {
		dump, _ := json.MarshalIndent(r, "", " ")
		t.Fatalf("verdict=%q exit=%d, want %q / %d\n%.4000s", r.Verdict, code, verdict, exit, dump)
	}
}
