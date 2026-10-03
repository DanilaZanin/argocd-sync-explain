//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/explain"
	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

const waitLong = 150 * time.Second

func opPhase(a *model.Application) string {
	if a.Status.OperationState == nil {
		return ""
	}
	return a.Status.OperationState.Phase
}

func opMsg(a *model.Application) string {
	if a.Status.OperationState == nil {
		return ""
	}
	return a.Status.OperationState.Message
}

func scenario(t *testing.T, name string, small bool, f func(t *testing.T)) {
	t.Helper()
	if !env.Full && !small {
		t.Run(name, func(t *testing.T) { t.Skip("full set only") })
		return
	}
	t.Run(name, f)
}

// TestScenarios is ordered: scenarios that need Argo CD without a health check for
// Application run first; the Lua health check is enabled for the later ones.
func TestScenarios(t *testing.T) {
	// ---- A11: wave-waiting-on-unhealthy-deployment (+ kubeconfig fallback, revision mismatch) ----
	scenario(t, "wave-waiting-on-unhealthy-deployment", true, func(t *testing.T) {
		o := appOpts{Name: "ase-wave", Path: "wave", Namespace: "ase-wave"}
		env.start(t, o, map[string]string{
			"web.yaml":   deployment("web", 0, false, 0),
			"later.yaml": configMap("later", 1, ""),
		}, true)
		env.waitApp(t, o.Name, "operation waiting on Deployment health", waitLong, func(a *model.Application) bool {
			return opPhase(a) == "Running" && strings.Contains(opMsg(a), "healthy state of apps/Deployment/web")
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
		b := mustBlocker(t, rep, explain.RoleWaitingOn, "resource", "Deployment", "web")
		if b.SyncPhase != "Sync" || b.Wave != "0" || b.Health == nil || b.Health.Status != "Progressing" {
			t.Fatalf("blocker details wrong: %+v health=%+v", b, b.Health)
		}
		if findBlocker(rep, "", "", "ConfigMap", "later") != nil {
			t.Fatal("a later-wave ConfigMap must not be a blocker")
		}
		if rep.Pending == nil || !contains(rep.Pending.Resources, "ConfigMap/later") {
			t.Fatalf("pending should list ConfigMap/later: %+v", rep.Pending)
		}
		podSeen := false
		for _, ob := range rep.Observation {
			if ob.Ref.Kind == "Pod" && ob.Under == "Deployment/web" {
				podSeen = true
			}
		}
		if !podSeen {
			t.Fatalf("expected a Pod observation under Deployment/web: %+v", rep.Observation)
		}
		if rep.Position == nil || rep.Position.Phase != "Sync" || rep.Position.Wave != "0" {
			t.Fatalf("position: %+v", rep.Position)
		}
		env.record(t, "wave-waiting", "app", o.Name)

		t.Run("kubeconfig-only-fallback", func(t *testing.T) {
			rep, code, _ := env.explain(t, "app", o.Name, "--mode", "kubeconfig", "--context", env.Context)
			expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
			b := mustBlocker(t, rep, explain.RoleWaitingOn, "resource", "Deployment", "web")
			if b.Health == nil || b.Health.Status != explain.Unknown {
				t.Fatalf("health must be unknown without the API: %+v", b.Health)
			}
			if rep.Mode != "kubeconfig" || !hasUnknown(rep, "tree") {
				t.Fatalf("tree must be an explicit unknown: %+v", rep.Unknowns)
			}
			if hasUnknown(rep, "events") || hasUnknown(rep, "version") || rep.ArgoVersion == "" {
				t.Fatalf("events and the server version are readable through the kubeconfig: %+v %q", rep.Unknowns, rep.ArgoVersion)
			}
			if len(rep.Observation) != 0 {
				t.Fatalf("no observations without the tree: %+v", rep.Observation)
			}
			env.record(t, "kubeconfig-only", "app", o.Name, "--mode", "kubeconfig", "--context", env.Context)
		})

		if env.Full {
			t.Run("revision-mismatch", func(t *testing.T) {
				// The waiting Deployment moves from wave 0 to wave 5 in a newer commit. The running
				// operation still uses the old revision, so the current comparison must not be read as its position.
				env.put(t, o.Path+"/web.yaml", deployment("web", 5, false, 0))
				env.refresh(o.Name)
				env.waitApp(t, o.Name, "comparison against a newer revision", 90*time.Second, func(a *model.Application) bool {
					return a.Status.OperationState != nil && a.Status.OperationState.SyncResult != nil &&
						a.Status.Sync.Revision != "" && a.Status.Sync.Revision != a.Status.OperationState.SyncResult.Revision &&
						a.Status.Resources != nil && waveOf(a, "web") == 5
				})
				rep, _, _ := env.explain(t, "app", o.Name)
				found := false
				for _, n := range rep.Notes {
					if n.Code == "revision-mismatch" {
						found = true
					}
				}
				if !found || rep.Verdict != explain.VerdictWaiting {
					t.Fatalf("revision mismatch note missing or verdict changed: %+v %s", rep.Notes, rep.Verdict)
				}
				b := mustBlocker(t, rep, explain.RoleWaitingOn, "resource", "Deployment", "web")
				if !strings.HasPrefix(b.Wave, explain.Unknown) || rep.Position == nil || rep.Position.Wave != explain.Unknown {
					t.Fatalf("the wave must be unknown, not 5: blocker=%q position=%+v", b.Wave, rep.Position)
				}
				if rep.Pending != nil || !hasUnknown(rep, "pending tasks") {
					t.Fatalf("pending must be unknown: %+v %+v", rep.Pending, rep.Unknowns)
				}
			})
		}
	})

	scenario(t, "healthy-synced", true, func(t *testing.T) {
		o := appOpts{Name: "ase-healthy", Path: "healthy", Namespace: "ase-healthy"}
		env.start(t, o, map[string]string{"cm.yaml": configMap("c", 0, ""), "web.yaml": deployment("ok", 0, true, 0)}, true)
		env.waitApp(t, o.Name, "Synced and Healthy", waitLong, func(a *model.Application) bool {
			return a.Status.Sync.Status == "Synced" && a.Status.Health.Status == "Healthy" && opPhase(a) == "Succeeded"
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictHealthy, 0)
		if len(rep.Blockers) != 0 {
			t.Fatalf("a healthy app has no blockers: %+v", rep.Blockers)
		}
		// Argo CD answers 403 for an application that does not exist; the text must not claim an RBAC cause.
		cmd := exec.Command(env.Bin, "-o", "json", "--insecure", "app", "does-not-exist")
		cmd.Env = append(os.Environ(), "ARGOCD_SERVER="+env.argoAdr, "ARGOCD_AUTH_TOKEN="+env.token)
		jout, jerr := cmd.Output()
		if ee, ok := jerr.(*exec.ExitError); !ok || ee.ExitCode() != 3 || !strings.Contains(string(jout), `"verdict": "unknown"`) || !strings.Contains(string(ee.Stderr), "denied or does not exist") {
			t.Fatalf("missing application: want exit 3 with a JSON unknown and the 403 wording: %v %s", jerr, jout)
		}
		// Same app through the tool's own --port-forward mode.
		out := env.text(t, "app", o.Name, "--port-forward", "--context", env.Context)
		if !strings.Contains(out, "VERDICT: HEALTHY") {
			t.Fatalf("--port-forward output:\n%s", out)
		}
	})

	scenario(t, "last-wave-progressing-not-blocked", false, func(t *testing.T) {
		o := appOpts{Name: "ase-lastwave", Path: "lastwave", Namespace: "ase-lastwave"}
		env.start(t, o, map[string]string{"web.yaml": deployment("web", 0, false, 0)}, true)
		env.waitApp(t, o.Name, "operation Succeeded while Progressing", waitLong, func(a *model.Application) bool {
			return opPhase(a) == "Succeeded" && a.Status.Health.Status == "Progressing"
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictProgressing, 2)
		if len(rep.Blockers) != 0 || rep.Operation.Kind != explain.OpFinished {
			t.Fatalf("a finished sync with a progressing rollout must have no blockers: %+v op=%s", rep.Blockers, rep.Operation.Kind)
		}
		if len(rep.Observation) == 0 {
			t.Fatal("expected the progressing Deployment as an observation")
		}
		env.record(t, "last-wave-progressing", "app", o.Name)
	})

	scenario(t, "presync-hook-running", false, func(t *testing.T) {
		o := appOpts{Name: "ase-hookrun", Path: "hookrun", Namespace: "ase-hookrun"}
		env.start(t, o, map[string]string{"hook.yaml": job("slow", "PreSync", "sleep 3600", 0, ""), "cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "operation waiting for the hook", waitLong, func(a *model.Application) bool {
			return opPhase(a) == "Running" && strings.Contains(opMsg(a), "completion of hook batch/Job/slow")
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
		b := mustBlocker(t, rep, explain.RoleWaitingOn, "hook", "Job", "slow")
		if b.HookType != "PreSync" || b.SyncPhase != "PreSync" || b.HookPhase != "Running" {
			t.Fatalf("hook blocker: %+v", b)
		}
		if findBlocker(rep, "", "", "ConfigMap", "c") != nil {
			t.Fatal("the ConfigMap is not started yet and must not be a blocker")
		}
		env.record(t, "presync-hook-running", "app", o.Name)
	})

	scenario(t, "presync-hook-failed", true, func(t *testing.T) {
		o := appOpts{Name: "ase-hookfail", Path: "hookfail", Namespace: "ase-hookfail"}
		env.start(t, o, map[string]string{"hook.yaml": job("bad", "PreSync", "echo boom; exit 1", 0, ""), "cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "operation Failed", waitLong, func(a *model.Application) bool { return opPhase(a) == "Failed" })
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictFailed, 2)
		b := mustBlocker(t, rep, explain.RoleFailed, "hook", "Job", "bad")
		if b.HookType != "PreSync" || (b.HookPhase != "Failed" && b.HookPhase != "Error") {
			t.Fatalf("failed hook: %+v", b)
		}
		env.record(t, "presync-hook-failed", "app", o.Name)
	})

	scenario(t, "postsync-hook-failed", false, func(t *testing.T) {
		o := appOpts{Name: "ase-postfail", Path: "postfail", Namespace: "ase-postfail"}
		env.start(t, o, map[string]string{"web.yaml": deployment("ok", 0, true, 0), "hook.yaml": job("post", "PostSync", "exit 3", 0, "")}, true)
		env.waitApp(t, o.Name, "operation Failed", waitLong, func(a *model.Application) bool { return opPhase(a) == "Failed" })
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictFailed, 2)
		b := mustBlocker(t, rep, explain.RoleFailed, "hook", "Job", "post")
		if b.HookType != "PostSync" || b.SyncPhase != "PostSync" {
			t.Fatalf("failed hook: %+v", b)
		}
	})

	scenario(t, "hook-deleted-after-success", false, func(t *testing.T) {
		o := appOpts{Name: "ase-hookdel", Path: "hookdel", Namespace: "ase-hookdel"}
		env.start(t, o, map[string]string{"hook.yaml": job("once", "PreSync", "exit 0", 0, "HookSucceeded"), "cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "Synced and Healthy", waitLong, func(a *model.Application) bool {
			return a.Status.Sync.Status == "Synced" && a.Status.Health.Status == "Healthy" && opPhase(a) == "Succeeded"
		})
		// The hook must really be gone (confirmed NotFound), and the operation must still remember it as Succeeded.
		if err := retry(60*time.Second, func() error {
			out, err := env.kubectl("", "-n", o.Namespace, "get", "job", "once", "--ignore-not-found", "-o", "name")
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) != "" {
				return fmt.Errorf("hook Job still exists: %s", out)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		a, _ := env.app(o.Name)
		remembered := false
		for _, x := range a.Status.OperationState.SyncResult.Resources {
			if x.Kind == "Job" && x.Name == "once" && x.HookType == "PreSync" && x.HookPhase == "Succeeded" {
				remembered = true
			}
		}
		if !remembered {
			t.Fatalf("the Succeeded hook result is expected in the operation history: %+v", a.Status.OperationState.SyncResult.Resources)
		}
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictHealthy, 0)
		if len(rep.Blockers) != 0 {
			t.Fatalf("a hook that finished and was deleted is history, not a blocker: %+v", rep.Blockers)
		}
		if out, _ := env.kubectl("", "-n", o.Namespace, "get", "job", "once", "--ignore-not-found", "-o", "name"); strings.TrimSpace(out) != "" {
			t.Fatalf("the hook must be NotFound: %s", out)
		}
		for _, o := range rep.Observation {
			if o.Ref.Name == "once" {
				t.Fatalf("a deleted hook must not appear in the tree observations: %+v", o)
			}
		}
	})

	scenario(t, "prune-confirm-waiting", false, func(t *testing.T) {
		o := appOpts{Name: "ase-prune", Path: "prune", Namespace: "ase-prune",
			SyncPolicy: "  syncPolicy:\n    automated: {prune: true}\n    retry: {limit: 0}\n"}
		env.start(t, o, map[string]string{
			"keep.yaml": configMap("keep", 0, ""),
			"gone.yaml": configMap("gone", 0, "    argocd.argoproj.io/sync-options: Prune=confirm"),
		}, true)
		env.waitApp(t, o.Name, "initial sync", waitLong, func(a *model.Application) bool {
			return a.Status.Sync.Status == "Synced" && opPhase(a) == "Succeeded"
		})
		env.remove(t, o.Path+"/gone.yaml")
		env.refresh(o.Name)
		env.waitApp(t, o.Name, "operation waiting for prune confirmation", waitLong, func(a *model.Application) bool {
			return opPhase(a) == "Running" && strings.Contains(opMsg(a), "pruning confirmation")
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
		mustBlocker(t, rep, explain.RoleWaitingOn, "prune-confirmation", "ConfigMap", "gone")
		if rep.Operation.WaitReason != "prune-confirmation" {
			t.Fatalf("wait reason: %s", rep.Operation.WaitReason)
		}
		env.record(t, "prune-confirm", "app", o.Name)
	})

	// Sync windows. The controller does not start an auto-sync while a window denies it and writes
	// nothing into the Application, so the explanation is the evaluated window. A manual operation
	// is created and held, with a message in the operation state.
	denyProject := func(t *testing.T, name, windows string) {
		env.apply(t, fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata: {name: %s, namespace: argocd}
spec:
  sourceRepos: ["*"]
  destinations: [{server: "*", namespace: "*"}]
  clusterResourceWhitelist: [{group: "*", kind: "*"}]
  syncWindows:
%s
`, name, windows))
		t.Cleanup(func() { _, _ = env.kubectl("", "-n", argoNS, "delete", "appproject", name, "--ignore-not-found") })
	}
	scenario(t, "sync-window-deny-now", true, func(t *testing.T) {
		denyProject(t, "ase-deny", `    - {kind: deny, schedule: "* * * * *", duration: 1h, applications: ["*"]}`)
		o := appOpts{Name: "ase-deny", Path: "deny", Namespace: "ase-deny", Project: "ase-deny"}
		env.start(t, o, map[string]string{"cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "OutOfSync", waitLong, func(a *model.Application) bool { return a.Status.Sync.Status == "OutOfSync" })
		time.Sleep(10 * time.Second) // the controller had its chance to auto-sync
		a, _ := env.app(o.Name)
		if a.Status.OperationState != nil {
			t.Fatalf("a deny window must keep auto-sync from starting an operation: %+v", a.Status.OperationState)
		}
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictOutOfSync, 2)
		if rep.SyncWindows == nil || rep.SyncWindows.CanSync || !strings.Contains(rep.SyncWindows.Reason, "deny window is active") {
			t.Fatalf("sync window evaluation: %+v", rep.SyncWindows)
		}
		if !hasBlockingFinding(rep, "sync window blocks automated syncs") {
			t.Fatalf("auto-sync findings: %+v", rep.AutoSync)
		}
		env.record(t, "sync-window-deny", "app", o.Name)
	})

	scenario(t, "no-allow-window", false, func(t *testing.T) {
		hour := (time.Now().UTC().Hour() + 12) % 24
		denyProject(t, "ase-noallow", fmt.Sprintf(`    - {kind: allow, schedule: "0 %d * * *", duration: 1h, applications: ["*"]}`, hour))
		o := appOpts{Name: "ase-noallow", Path: "noallow", Namespace: "ase-noallow", Project: "ase-noallow"}
		env.start(t, o, map[string]string{"cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "OutOfSync", waitLong, func(a *model.Application) bool { return a.Status.Sync.Status == "OutOfSync" })
		time.Sleep(10 * time.Second)
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictOutOfSync, 2)
		if rep.SyncWindows == nil || rep.SyncWindows.CanSync || !strings.Contains(rep.SyncWindows.Reason, "none is active") {
			t.Fatalf("sync window evaluation: %+v", rep.SyncWindows)
		}
		if !hasBlockingFinding(rep, "sync window blocks automated syncs") {
			t.Fatalf("auto-sync findings: %+v", rep.AutoSync)
		}
	})

	scenario(t, "sync-window-holds-manual-operation", false, func(t *testing.T) {
		denyProject(t, "ase-denym", `    - {kind: deny, schedule: "* * * * *", duration: 1h, applications: ["*"]}`)
		o := appOpts{Name: "ase-denym", Path: "denym", Namespace: "ase-denym", Project: "ase-denym", SyncPolicy: "  syncPolicy:\n    syncOptions: []\n"}
		env.start(t, o, map[string]string{"cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "OutOfSync", waitLong, func(a *model.Application) bool { return a.Status.Sync.Status == "OutOfSync" })
		if out, err := env.kubectl("", "-n", argoNS, "patch", "application", o.Name, "--type=merge", "-p",
			`{"operation":{"initiatedBy":{"username":"e2e"},"sync":{"revision":"main"}}}`); err != nil {
			t.Fatalf("request sync: %v %s", err, out)
		}
		env.waitApp(t, o.Name, "operation held by the sync window", waitLong, func(a *model.Application) bool {
			return strings.Contains(opMsg(a), "blocked by sync window")
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
		mustBlocker(t, rep, explain.RoleWaitingOn, "sync-window", "SyncWindow", "")
		if rep.SyncWindows == nil || rep.SyncWindows.CanSync || !rep.SyncWindows.Manual {
			t.Fatalf("sync window evaluation: %+v", rep.SyncWindows)
		}
		env.record(t, "sync-window-holds-operation", "app", o.Name)
	})

	scenario(t, "automated-disabled", true, func(t *testing.T) {
		o := appOpts{Name: "ase-noauto", Path: "noauto", Namespace: "ase-noauto", SyncPolicy: "  syncPolicy:\n    syncOptions: []\n"}
		env.start(t, o, map[string]string{"cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "OutOfSync", waitLong, func(a *model.Application) bool { return a.Status.Sync.Status == "OutOfSync" })
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictOutOfSync, 2)
		if rep.AutoSync == nil || rep.AutoSync.Enabled || len(rep.AutoSync.Verified) == 0 || !strings.Contains(rep.AutoSync.Verified[0].Text, "not configured") {
			t.Fatalf("auto-sync: %+v", rep.AutoSync)
		}
		if len(rep.OutOfSync) != 1 || rep.OutOfSync[0].Ref.Name != "c" || !strings.HasPrefix(rep.OutOfSync[0].Missing, "yes") {
			t.Fatalf("out of sync list: %+v", rep.OutOfSync)
		}
		env.record(t, "automated-disabled", "app", o.Name)
	})

	scenario(t, "automated-enabled-false", false, func(t *testing.T) {
		o := appOpts{Name: "ase-enabledfalse", Path: "enabledfalse", Namespace: "ase-enabledfalse",
			SyncPolicy: "  syncPolicy:\n    automated: {enabled: false}\n"}
		env.start(t, o, map[string]string{"cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "OutOfSync", waitLong, func(a *model.Application) bool { return a.Status.Sync.Status == "OutOfSync" })
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictOutOfSync, 2)
		if rep.AutoSync == nil || rep.AutoSync.Enabled || !strings.Contains(rep.AutoSync.Verified[0].Text, "disabled") {
			t.Fatalf("auto-sync: %+v", rep.AutoSync)
		}
	})

	scenario(t, "retry-backoff", false, func(t *testing.T) {
		o := appOpts{Name: "ase-backoff", Path: "backoff", Namespace: "ase-backoff",
			SyncPolicy: "  syncPolicy:\n    automated: {}\n    retry: {limit: 5, backoff: {duration: 1h, factor: 2, maxDuration: 2h}}\n"}
		env.start(t, o, map[string]string{"hook.yaml": job("bad", "PreSync", "exit 1", 0, ""), "cm.yaml": configMap("c", 0, "")}, true)
		a := env.waitApp(t, o.Name, "retry backoff (Running with finishedAt)", waitLong, func(a *model.Application) bool {
			return a.Status.OperationState != nil && a.Status.OperationState.Phase == "Running" && a.Status.OperationState.FinishedAt != nil
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictRetrying, 2)
		if rep.Operation.Kind != explain.OpBackoff || rep.Operation.Retry == nil || rep.Operation.Retry.Limit != 5 || rep.Operation.Retry.Unlimited {
			t.Fatalf("operation: %+v", rep.Operation)
		}
		want := a.Status.OperationState.FinishedAt.UTC().Add(2 * time.Hour).Format(time.RFC3339)
		if rep.Operation.NextRetryNotBefore != want {
			t.Fatalf("nextRetryNotBefore=%s want %s (retryCount=%d)", rep.Operation.NextRetryNotBefore, want, a.Status.OperationState.RetryCount)
		}
		mustBlocker(t, rep, explain.RoleFailed, "hook", "Job", "bad")
		env.record(t, "retry-backoff", "app", o.Name)
	})

	// Argo CD's own message announces the retry time computed with the old retryCount, while the
	// controller resumes with the incremented one. The tool reports the later, enforced time as
	// "not before"; this scenario observes the real controller to confirm that it does not retry earlier.
	scenario(t, "retry-backoff-timing", false, func(t *testing.T) {
		o := appOpts{Name: "ase-backofft", Path: "backofft", Namespace: "ase-backofft",
			SyncPolicy: "  syncPolicy:\n    automated: {}\n    retry: {limit: 5, backoff: {duration: 20s, factor: 2, maxDuration: 10m}}\n"}
		env.start(t, o, map[string]string{"hook.yaml": job("bad", "PreSync", "exit 1", 0, ""), "cm.yaml": configMap("c", 0, "")}, true)
		a := env.waitApp(t, o.Name, "first failure in retry backoff", waitLong, func(a *model.Application) bool {
			return a.Status.OperationState != nil && a.Status.OperationState.Phase == "Running" &&
				a.Status.OperationState.FinishedAt != nil && a.Status.OperationState.RetryCount == 1
		})
		finished := a.Status.OperationState.FinishedAt.UTC()
		rep, _, _ := env.explain(t, "app", o.Name)
		want := finished.Add(40 * time.Second).Format(time.RFC3339)
		if rep.Operation.Kind != explain.OpBackoff || rep.Operation.NextRetryNotBefore != want {
			t.Fatalf("kind=%s next=%s want %s", rep.Operation.Kind, rep.Operation.NextRetryNotBefore, want)
		}
		// The new attempt is identified by the creationTimestamp of the recreated hook Job, which is the moment
		// the retry really started, not the moment this test noticed it.
		first := env.jobCreated(t, o.Namespace, "bad")
		var second time.Time
		if err := retry(3*time.Minute, func() error {
			ts := env.jobCreated(t, o.Namespace, "bad")
			if ts.IsZero() || ts.Equal(first) {
				return fmt.Errorf("no new attempt yet")
			}
			second = ts
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		notBefore, err := time.Parse(time.RFC3339, rep.Operation.NextRetryNotBefore)
		if err != nil {
			t.Fatal(err)
		}
		if second.Before(notBefore) {
			t.Fatalf("the new attempt started at %s, before the reported not-before %s", second.Format(time.RFC3339), notBefore.Format(time.RFC3339))
		}
	})

	// Opus B1: CreateNamespace=true leaves a Running result for the synthetic Namespace next to the real PreSync hook.
	scenario(t, "createnamespace-with-presync-hook", false, func(t *testing.T) {
		o := appOpts{Name: "ase-nshook", Path: "nshook", Namespace: "ase-nshook",
			SyncPolicy: "  syncPolicy:\n    automated: {}\n    retry: {limit: 0}\n    syncOptions: [CreateNamespace=true]\n"}
		env.namespaceCleanup(t, o.Namespace)
		env.start(t, o, map[string]string{"hook.yaml": job("slow", "PreSync", "sleep 3600", 0, ""), "cm.yaml": configMap("c", 0, "")}, false)
		env.waitApp(t, o.Name, "operation waiting for the hook", waitLong, func(a *model.Application) bool {
			return opPhase(a) == "Running" && strings.Contains(opMsg(a), "completion of hook batch/Job/slow")
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
		mustBlocker(t, rep, explain.RoleWaitingOn, "hook", "Job", "slow")
		for _, b := range rep.Blockers {
			if b.Role == explain.RoleWaitingOn && b.Ref.Kind == "Namespace" {
				t.Fatalf("the synthetic Namespace result is stale, not a blocker: %+v", rep.Blockers)
			}
		}
		if hasUnknown(rep, "which tasks the operation waits on") {
			t.Fatalf("one task is named and one is proven: %+v", rep.Unknowns)
		}
		env.record(t, "createnamespace-hook", "app", o.Name)
	})

	// PruneLast: the prune that is still ahead explains why the operation is not finished.
	scenario(t, "prune-last-pending", false, func(t *testing.T) {
		o := appOpts{Name: "ase-prunelast", Path: "prunelast", Namespace: "ase-prunelast",
			SyncPolicy: "  syncPolicy:\n    automated: {prune: true}\n    retry: {limit: 0}\n"}
		env.start(t, o, map[string]string{
			"web.yaml": deployment("web", 0, false, 0),
			"old.yaml": configMap("old", 0, "    argocd.argoproj.io/sync-options: PruneLast=true"),
		}, true)
		env.waitApp(t, o.Name, "initial sync", waitLong, func(a *model.Application) bool {
			return a.Status.Sync.Status == "Synced" && opPhase(a) == "Succeeded"
		})
		env.remove(t, o.Path+"/old.yaml")
		env.put(t, o.Path+"/web.yaml", strings.Replace(deployment("web", 0, false, 0), "sleep\", \"3600", "sleep\", \"3601", 1))
		env.refresh(o.Name)
		env.waitApp(t, o.Name, "operation waiting on the Deployment with a PruneLast task ahead", waitLong, func(a *model.Application) bool {
			return opPhase(a) == "Running" && strings.Contains(opMsg(a), "healthy state of apps/Deployment/web")
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
		mustBlocker(t, rep, explain.RoleWaitingOn, "resource", "Deployment", "web")
		if rep.Pending == nil || !contains(rep.Pending.Resources, "ConfigMap/old (to prune)") {
			t.Fatalf("the PruneLast task must be pending: %+v", rep.Pending)
		}
	})

	// BeforeHookCreation: the previous hook object is being deleted before the hook runs again.
	scenario(t, "hook-deletion-wait", false, func(t *testing.T) {
		o := appOpts{Name: "ase-hookdelwait", Path: "hookdelwait", Namespace: "ase-hookdelwait"}
		env.start(t, o, map[string]string{"hook.yaml": job("once", "PreSync", "exit 0", 0, ""), "cm.yaml": configMap("c", 0, "")}, true)
		env.waitApp(t, o.Name, "first sync", waitLong, func(a *model.Application) bool {
			return a.Status.Sync.Status == "Synced" && opPhase(a) == "Succeeded"
		})
		// A finalizer of our own keeps the old hook object from going away.
		if out, err := env.kubectl("", "-n", o.Namespace, "patch", "job", "once", "--type=merge", "-p", `{"metadata":{"finalizers":["ase/hold"]}}`); err != nil {
			t.Fatalf("hold hook: %v %s", err, out)
		}
		t.Cleanup(func() {
			_, _ = env.kubectl("", "-n", o.Namespace, "patch", "job", "once", "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		})
		env.put(t, o.Path+"/cm.yaml", configMap("c", 0, "    changed: \"yes\""))
		env.refresh(o.Name)
		env.waitApp(t, o.Name, "operation waiting for the old hook to be deleted", waitLong, func(a *model.Application) bool {
			return opPhase(a) == "Running" && strings.Contains(opMsg(a), "deletion of hook")
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
		if rep.Operation.WaitReason != "hook-deletion" {
			t.Fatalf("wait reason: %s", rep.Operation.WaitReason)
		}
		b := mustBlocker(t, rep, explain.RoleWaitingOn, "hook", "Job", "once")
		if strings.Contains(b.Why, "pruned") {
			t.Fatalf("a hook deletion is not a pruned resource: %s", b.Why)
		}
	})

	// Opus B2: the Application is being deleted and a managed resource holds a finalizer.
	scenario(t, "deleting-held-by-finalizer", false, func(t *testing.T) {
		o := appOpts{Name: "ase-deleting", Path: "deleting", Namespace: "ase-deleting", Finalizers: true}
		env.start(t, o, map[string]string{"cm.yaml": configMap("held", 0, "")}, true)
		env.waitApp(t, o.Name, "Synced", waitLong, func(a *model.Application) bool {
			return a.Status.Sync.Status == "Synced" && opPhase(a) == "Succeeded"
		})
		if out, err := env.kubectl("", "-n", o.Namespace, "patch", "configmap", "held", "--type=merge", "-p", `{"metadata":{"finalizers":["ase/hold"]}}`); err != nil {
			t.Fatalf("hold: %v %s", err, out)
		}
		t.Cleanup(func() {
			_, _ = env.kubectl("", "-n", o.Namespace, "patch", "configmap", "held", "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		})
		if out, err := env.kubectl("", "-n", argoNS, "delete", "application", o.Name, "--wait=false"); err != nil {
			t.Fatalf("delete: %v %s", err, out)
		}
		env.waitApp(t, o.Name, "deletionTimestamp on the Application", waitLong, func(a *model.Application) bool { return a.Metadata.DeletionTimestamp != nil })
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictDeleting, 2)
		b := mustBlocker(t, rep, explain.RoleWaitingOn, "resource", "ConfigMap", "held")
		if !strings.Contains(b.Why, "ase/hold") {
			t.Fatalf("the holding finalizer must be named: %s", b.Why)
		}
		if rep.Deleting == nil || !strings.HasPrefix(rep.Deleting.Cascade, "cascade") || len(rep.Deleting.Finalizers) == 0 {
			t.Fatalf("%+v", rep.Deleting)
		}
		env.record(t, "deleting", "app", o.Name)
	})

	// Opus round 2: a failed apply and a SyncFail hook that is still running. The headline and the position
	// come from the running hook only; the Deployment is listed as failed.
	scenario(t, "syncfail-hook-running-after-failed-apply", false, func(t *testing.T) {
		o := appOpts{Name: "ase-syncfail", Path: "syncfail", Namespace: "ase-syncfail"}
		neg := strings.Replace(deployment("neg", 0, true, 0), "replicas: 1", "replicas: -1", 1)
		env.start(t, o, map[string]string{"neg.yaml": neg, "onfail.yaml": job("onfail", "SyncFail", "sleep 3600", 0, "")}, true)
		env.waitApp(t, o.Name, "operation waiting for the SyncFail hook", waitLong, func(a *model.Application) bool {
			return opPhase(a) == "Running" && strings.Contains(opMsg(a), "completion of hook batch/Job/onfail")
		})
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
		b := mustBlocker(t, rep, explain.RoleWaitingOn, "hook", "Job", "onfail")
		if b.HookType != "SyncFail" {
			t.Fatalf("%+v", b)
		}
		mustBlocker(t, rep, explain.RoleFailed, "resource", "Deployment", "neg")
		if strings.Contains(rep.Summary, "Deployment/neg,") || strings.Contains(rep.Summary, ", Deployment/neg") || !strings.Contains(rep.Summary, "Already failed: Deployment/neg") {
			t.Fatalf("headline: %s", rep.Summary)
		}
		if rep.Position == nil || rep.Position.Phase != "SyncFail" {
			t.Fatalf("position must be the running SyncFail task only: %+v", rep.Position)
		}
		env.record(t, "syncfail-running", "app", o.Name)
	})

	scenario(t, "comparison-error-invalid-manifest", false, func(t *testing.T) {
		o := appOpts{Name: "ase-invalid", Path: "invalid", Namespace: "ase-invalid"}
		env.start(t, o, map[string]string{"bad.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: [unterminated\n"}, true)
		env.waitApp(t, o.Name, "ComparisonError", waitLong, func(a *model.Application) bool { return hasCond(a, "ComparisonError") })
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictError, 2)
		if !hasReportCond(rep, "ComparisonError") {
			t.Fatalf("conditions: %+v", rep.Conditions)
		}
		env.record(t, "comparison-error", "app", o.Name)
	})

	scenario(t, "repo-unreachable", false, func(t *testing.T) {
		o := appOpts{Name: "ase-norepo", Path: "x", Namespace: "ase-norepo", RepoURL: "http://nonexistent.git.svc.cluster.local:3000/ase/none.git"}
		env.namespace(t, o.Namespace)
		env.apply(t, appYAML(o))
		t.Cleanup(func() { env.deleteApp(o.Name) })
		env.waitApp(t, o.Name, "ComparisonError", waitLong, func(a *model.Application) bool { return hasCond(a, "ComparisonError") })
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictError, 2)
	})

	scenario(t, "namespace-missing", false, func(t *testing.T) {
		o := appOpts{Name: "ase-nons", Path: "nons", Namespace: "ase-does-not-exist", SyncPolicy: "  syncPolicy:\n    syncOptions: []\n"}
		env.start(t, o, map[string]string{"cm.yaml": configMap("c", 0, "")}, false)
		env.waitApp(t, o.Name, "OutOfSync", waitLong, func(a *model.Application) bool { return a.Status.Sync.Status == "OutOfSync" })
		// A manual sync request: the same .operation field the UI and CLI write.
		if out, err := env.kubectl("", "-n", argoNS, "patch", "application", o.Name, "--type=merge", "-p",
			`{"operation":{"initiatedBy":{"username":"e2e"},"retry":{"limit":0},"sync":{"revision":"main"}}}`); err != nil {
			t.Fatalf("request sync: %v %s", err, out)
		}
		env.waitApp(t, o.Name, "operation Failed", waitLong, func(a *model.Application) bool { return opPhase(a) == "Failed" })
		rep, code, _ := env.explain(t, "app", o.Name)
		expectVerdict(t, rep, code, explain.VerdictFailed, 2)
		b := mustBlocker(t, rep, explain.RoleFailed, "resource", "ConfigMap", "c")
		if !strings.Contains(strings.ToLower(b.Message), "namespace") {
			t.Fatalf("failure message should mention the namespace: %+v", b)
		}
		env.record(t, "namespace-missing", "app", o.Name)
	})

	// ---- app-of-apps without an Application health check ----
	scenario(t, "child-app-progressing-without-lua", false, func(t *testing.T) {
		parent, child := "ase-nolua-parent", "ase-nolua-child"
		env.namespace(t, "ase-nolua-child")
		env.put(t, "nolua-child/web.yaml", deployment("web", 0, false, 0))
		env.put(t, "nolua-parent/child.yaml", childApp(child, "nolua-child"))
		env.put(t, "nolua-parent/after.yaml", configMap("after-nolua", 1, ""))
		o := appOpts{Name: parent, Path: "nolua-parent", Namespace: "argocd"}
		env.start(t, o, nil, false)
		t.Cleanup(func() { env.deleteApp(child) })
		env.waitApp(t, parent, "parent operation Succeeded", waitLong, func(a *model.Application) bool { return opPhase(a) == "Succeeded" })
		env.waitApp(t, child, "child Progressing", waitLong, func(a *model.Application) bool { return a.Status.Health.Status == "Progressing" })
		rep, code, _ := env.explain(t, "app", parent)
		if rep.Verdict != explain.VerdictHealthy || len(rep.Blockers) != 0 {
			t.Fatalf("the parent does not wait for the child: %s %+v", rep.Verdict, rep.Blockers)
		}
		if len(rep.Children) != 1 || rep.Children[0].Relation != explain.RelNotWaiting {
			t.Fatalf("child relation: %+v", rep.Children)
		}
		cr := rep.Children[0].Report
		if cr == nil || cr.Verdict != explain.VerdictProgressing {
			t.Fatalf("the child is explained independently: %+v", cr)
		}
		if code != 2 {
			t.Fatalf("exit code must reflect the unhealthy child, got %d", code)
		}
		env.record(t, "app-of-apps-without-lua", "app", parent)
	})

	// ---- app-of-apps with the Application health check ----
	enableLua := func(t *testing.T) {
		env.setCM(t, `{"data":{"resource.customizations.health.argoproj.io_Application":"hs = {}\nhs.status = \"Progressing\"\nhs.message = \"\"\nif obj.status ~= nil then\n  if obj.status.health ~= nil then\n    hs.status = obj.status.health.status\n    if obj.status.health.message ~= nil then\n      hs.message = obj.status.health.message\n    end\n  end\nend\nreturn hs\n"}}`)
		time.Sleep(5 * time.Second)
	}
	scenario(t, "child-app-with-lua", false, func(t *testing.T) {
		enableLua(t)
		t.Run("progressing-child-parent-waits", func(t *testing.T) {
			parent, child := "ase-lua-parent", "ase-lua-child"
			env.namespace(t, "ase-lua-child")
			env.put(t, "lua-child/web.yaml", deployment("web", 0, false, 0))
			env.put(t, "lua-parent/child.yaml", childApp(child, "lua-child"))
			env.put(t, "lua-parent/after.yaml", configMap("after-lua", 1, ""))
			o := appOpts{Name: parent, Path: "lua-parent", Namespace: "argocd"}
			env.start(t, o, nil, false)
			t.Cleanup(func() { env.deleteApp(child) })
			env.waitApp(t, parent, "parent waiting on the child", waitLong, func(a *model.Application) bool {
				return opPhase(a) == "Running" && strings.Contains(opMsg(a), "argoproj.io/Application/"+child)
			})
			rep, code, _ := env.explain(t, "app", parent)
			expectVerdict(t, rep, code, explain.VerdictWaiting, 2)
			b := mustBlocker(t, rep, explain.RoleWaitingOn, "child-application", "Application", child)
			if b.Health == nil || b.Health.Status != "Progressing" {
				t.Fatalf("child health: %+v", b.Health)
			}
			if len(rep.Children) != 1 || rep.Children[0].Relation != explain.RelWaits || rep.Children[0].Report == nil {
				t.Fatalf("children: %+v", rep.Children)
			}
			if findBlocker(rep, "", "", "ConfigMap", "after-lua") != nil {
				t.Fatal("the parent's next-wave ConfigMap is not a blocker")
			}
			env.record(t, "app-of-apps-with-lua", "app", parent)
		})
		t.Run("degraded-child-fails-parent", func(t *testing.T) {
			parent, child := "ase-luad-parent", "ase-luad-child"
			env.namespace(t, "ase-luad-child")
			env.put(t, "luad-child/web.yaml", deployment("web", 0, false, 10))
			env.put(t, "luad-parent/child.yaml", childApp(child, "luad-child"))
			env.put(t, "luad-parent/after.yaml", configMap("after-luad", 1, ""))
			o := appOpts{Name: parent, Path: "luad-parent", Namespace: "argocd"}
			env.start(t, o, nil, false)
			t.Cleanup(func() { env.deleteApp(child) })
			env.waitApp(t, parent, "parent operation Failed", 200*time.Second, func(a *model.Application) bool { return opPhase(a) == "Failed" })
			rep, code, _ := env.explain(t, "app", parent)
			expectVerdict(t, rep, code, explain.VerdictFailed, 2)
			mustBlocker(t, rep, explain.RoleFailed, "child-application", "Application", child)
		})
	})

	// ---- RBAC-limited token ----
	scenario(t, "rbac-limited", false, func(t *testing.T) {
		parent, child := "ase-rbac-parent", "ase-rbac-child"
		env.namespace(t, "ase-rbac-child")
		env.put(t, "rbac-child/cm.yaml", configMap("c", 0, ""))
		env.put(t, "rbac-parent/child.yaml", childApp(child, "rbac-child"))
		o := appOpts{Name: parent, Path: "rbac-parent", Namespace: "argocd"}
		env.start(t, o, nil, false)
		t.Cleanup(func() { env.deleteApp(child) })
		env.waitApp(t, parent, "parent synced", waitLong, func(a *model.Application) bool { return a.Status.Sync.Status == "Synced" && opPhase(a) == "Succeeded" })
		env.waitApp(t, child, "child synced", waitLong, func(a *model.Application) bool { return a.Status.Sync.Status == "Synced" })

		env.setCM(t, `{"data":{"accounts.ase-limited":"apiKey"}}`)
		if out, err := env.kubectl("", "-n", argoNS, "patch", "configmap", "argocd-rbac-cm", "--type=merge", "-p",
			`{"data":{"policy.csv":"p, role:ase-limited, applications, get, default/ase-rbac-parent, allow\ng, ase-limited, role:ase-limited\n"}}`); err != nil {
			t.Fatalf("rbac cm: %v %s", err, out)
		}
		tok, err := env.accountToken("ase-limited")
		if err != nil {
			t.Fatal(err)
		}
		rep, code := env.explainWithToken(t, tok, "app", parent)
		if rep.Verdict != explain.VerdictHealthy || code != 3 {
			t.Fatalf("parent healthy but the child is unreadable: want exit 3, got verdict=%s exit=%d", rep.Verdict, code)
		}
		if !hasUnknown(rep, "child application argocd/"+child) || !hasUnknown(rep, "project") {
			t.Fatalf("unknowns must name the unreadable child and project: %+v", rep.Unknowns)
		}
		for _, u := range rep.Unknowns {
			if strings.Contains(u.What, "child") && !strings.Contains(u.Reason, "permission denied") {
				t.Fatalf("reason should say permission denied: %+v", u)
			}
		}
		// An Application the token cannot read at all is an explicit unknown (exit 3), not a tool error.
		cmd := exec.Command(env.Bin, "-o", "json", "--insecure", "app", child)
		cmd.Env = append(os.Environ(), "ARGOCD_SERVER="+env.argoAdr, "ARGOCD_AUTH_TOKEN="+tok)
		out, err := cmd.CombinedOutput()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 3 || !strings.Contains(string(out), "denied or does not exist") {
			t.Fatalf("unreadable root application: want exit 3, got %v: %s", err, out)
		}
		env.record(t, "rbac-limited", "app", parent)
	})
}

// ---- small helpers ----

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func hasUnknown(r *explain.Report, what string) bool {
	for _, u := range r.Unknowns {
		if u.What == what {
			return true
		}
	}
	return false
}

func hasCond(a *model.Application, typ string) bool {
	for _, c := range a.Status.Conditions {
		if c.Type == typ {
			return true
		}
	}
	return false
}

func hasReportCond(r *explain.Report, typ string) bool {
	for _, c := range r.Conditions {
		if c.Type == typ {
			return true
		}
	}
	return false
}

func hasBlockingFinding(r *explain.Report, sub string) bool {
	if r.AutoSync == nil {
		return false
	}
	for _, f := range r.AutoSync.Verified {
		if f.Blocks && strings.Contains(f.Text, sub) {
			return true
		}
	}
	return false
}

func waveOf(a *model.Application, name string) int64 {
	for _, r := range a.Status.Resources {
		if r.Name == name {
			return r.SyncWave
		}
	}
	return -1
}
