package explain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// snap builds a snapshot from a JSON Application fragment so that tests read like the real data.
func snap(t *testing.T, mode, appJSON string) *model.Snapshot {
	t.Helper()
	var a model.Application
	if err := json.Unmarshal([]byte(appJSON), &a); err != nil {
		t.Fatalf("bad test app JSON: %v", err)
	}
	if a.Metadata.Namespace == "" {
		a.Metadata.Namespace = "argocd"
	}
	return &model.Snapshot{Mode: mode, Now: now, ArgoVersion: "v3.5.3", App: a,
		Children: map[string]*model.Snapshot{}, Unavailable: map[string]string{}}
}

func hasBlocker(r *Report, role, typ, kind, name string) bool {
	for _, b := range r.Blockers {
		if b.Role == role && b.Type == typ && b.Ref.Kind == kind && b.Ref.Name == name {
			return true
		}
	}
	return false
}

func hasNote(r *Report, code string) bool {
	for _, n := range r.Notes {
		if n.Code == code {
			return true
		}
	}
	return false
}

func hasUnknownItem(r *Report, what string) bool {
	for _, u := range r.Unknowns {
		if u.What == what {
			return true
		}
	}
	return false
}

func verified(r *Report, sub string) bool {
	if r.AutoSync == nil {
		return false
	}
	for _, f := range r.AutoSync.Verified {
		if strings.Contains(f.Text, sub) {
			return true
		}
	}
	return false
}

func possible(r *Report, sub string) bool {
	if r.AutoSync == nil {
		return false
	}
	for _, f := range r.AutoSync.Possible {
		if strings.Contains(f.Text, sub) {
			return true
		}
	}
	return false
}

const base = `"metadata":{"name":"web","namespace":"argocd"},
 "spec":{"project":"default","source":{"repoURL":"http://git/x.git","path":"p","targetRevision":"main"},"destination":{"server":"https://kubernetes.default.svc","namespace":"web"}}`

func TestOperationClassification(t *testing.T) {
	cases := []struct {
		name string
		app  string
		kind string
		verd string
		exit int
	}{
		{"no operation, synced, healthy", `{` + base + `,"status":{"sync":{"status":"Synced","revision":"aaa"},"health":{"status":"Healthy"}}}`, OpNone, VerdictHealthy, 0},
		{"queued: .operation set and the previous state is finished", `{` + base + `,"operation":{"sync":{}},"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Healthy"},"operationState":{"phase":"Succeeded","startedAt":"2026-10-03T10:00:00Z","operation":{"sync":{}}}}}`, OpQueued, VerdictQueued, 2},
		{"queued: .operation set and no state at all", `{` + base + `,"operation":{"sync":{}},"status":{"sync":{"status":"OutOfSync"}}}`, OpQueued, VerdictQueued, 2},
		{"running: .operation set and state Running is the same operation", `{` + base + `,"operation":{"sync":{}},"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Running","message":"waiting for healthy state of apps/Deployment/web","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},"syncResult":{"revision":"aaa","resources":[{"group":"apps","kind":"Deployment","namespace":"web","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"Sync"}]}}}}`, OpRunning, VerdictWaiting, 2},
		{"retry backoff: Running with finishedAt", `{` + base + `,"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Running","message":"x. Retrying attempt #1 at 1:00PM.","startedAt":"2026-10-03T11:50:00Z","finishedAt":"2026-10-03T11:59:00Z","retryCount":1,"operation":{"retry":{"limit":5},"sync":{}}}}}`, OpBackoff, VerdictRetrying, 2},
		{"terminating", `{` + base + `,"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Terminating","startedAt":"2026-10-03T11:50:00Z","operation":{"sync":{}}}}}`, OpTerminating, VerdictTerminating, 2},
		{"finished failed", `{` + base + `,"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Failed","message":"one or more objects failed to apply","startedAt":"2026-10-03T11:50:00Z","finishedAt":"2026-10-03T11:51:00Z","operation":{"sync":{}}}}}`, OpFinished, VerdictFailed, 2},
		{"finished error", `{` + base + `,"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Error","message":"boom","startedAt":"2026-10-03T11:50:00Z","finishedAt":"2026-10-03T11:51:00Z","operation":{"sync":{}}}}}`, OpFinished, VerdictFailed, 2},
		{"finished succeeded, progressing", `{` + base + `,"status":{"sync":{"status":"Synced"},"health":{"status":"Progressing"},"operationState":{"phase":"Succeeded","startedAt":"2026-10-03T11:50:00Z","finishedAt":"2026-10-03T11:51:00Z","operation":{"sync":{}}}}}`, OpFinished, VerdictProgressing, 2},
		{"synced, degraded, no operation", `{` + base + `,"status":{"sync":{"status":"Synced"},"health":{"status":"Degraded"}}}`, OpNone, VerdictDegraded, 2},
		{"no status at all", `{` + base + `}`, OpNone, VerdictUnknown, 3},
		{"comparison error", `{` + base + `,"status":{"sync":{"status":"Unknown"},"conditions":[{"type":"ComparisonError","message":"boom"}]}}`, OpNone, VerdictError, 2},
		{"sync Unknown without a condition", `{` + base + `,"status":{"sync":{"status":"Unknown"},"health":{"status":"Healthy"}}}`, OpNone, VerdictUnknown, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Explain(snap(t, "api", c.app))
			if r.Operation.Kind != c.kind || r.Verdict != c.verd || r.ExitCode != c.exit {
				t.Fatalf("kind=%s verdict=%s exit=%d, want %s/%s/%d\n%s", r.Operation.Kind, r.Verdict, r.ExitCode, c.kind, c.verd, c.exit, r.Summary)
			}
		})
	}
}

func TestBackoffNextRetryUsesStoredRetryCount(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Running","startedAt":"2026-10-03T11:50:00Z","finishedAt":"2026-10-03T11:59:00Z","retryCount":1,
	  "operation":{"retry":{"limit":-1,"backoff":{"duration":"1m","factor":3,"maxDuration":"1h"}},"sync":{}}}}}`)
	r := Explain(s)
	if r.Operation.NextRetryNotBefore != "2026-10-03T12:02:00Z" {
		t.Fatalf("next retry %s (1m * 3^1 after 11:59)", r.Operation.NextRetryNotBefore)
	}
	if r.Operation.Retry == nil || !r.Operation.Retry.Unlimited {
		t.Fatalf("limit -1 is unlimited: %+v", r.Operation.Retry)
	}
}

func TestRunningWaitsUseControllerMessageAndIgnoreStaleResults(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync","revision":"bbb"},"health":{"status":"Progressing"},
	 "resources":[{"kind":"ConfigMap","name":"later","namespace":"web","status":"OutOfSync","syncWave":1},{"group":"apps","kind":"Deployment","name":"web","namespace":"web","status":"Synced"}],
	 "operationState":{"phase":"Running","message":"waiting for healthy state of apps/Deployment/web","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},
	  "syncResult":{"revision":"aaa","resources":[
	    {"kind":"Namespace","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"PreSync"},
	    {"group":"apps","kind":"Deployment","namespace":"web","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"Sync"}]}}}}`)
	s.Tree = &model.ResourceTree{Nodes: []model.TreeNode{
		{NodeRef: model.NodeRef{Group: "apps", Kind: "Deployment", Namespace: "web", Name: "web", UID: "d"}, Health: &model.Health{Status: "Progressing", Message: "Waiting for rollout"}},
		{NodeRef: model.NodeRef{Group: "apps", Kind: "ReplicaSet", Namespace: "web", Name: "web-1", UID: "r"}, ParentRefs: []model.NodeRef{{UID: "d"}}, Health: &model.Health{Status: "Progressing"}},
		{NodeRef: model.NodeRef{Kind: "Pod", Namespace: "web", Name: "web-1-x", UID: "p"}, ParentRefs: []model.NodeRef{{UID: "r"}}, Health: &model.Health{Status: "Progressing"},
			Info: []model.InfoItem{{Name: "Status Reason", Value: "ImagePullBackOff"}}},
	}}
	r := Explain(s)
	if len(r.Blockers) != 1 || !hasBlocker(r, RoleWaitingOn, "resource", "Deployment", "web") {
		t.Fatalf("exactly the Deployment is the blocker: %+v", r.Blockers)
	}
	if !hasNote(r, "stale-running-result") || !hasNote(r, "revision-mismatch") {
		t.Fatalf("notes: %+v", r.Notes)
	}
	if r.Position.Phase != "Sync" || r.Position.Wave != "0" {
		t.Fatalf("position: %+v", r.Position)
	}
	if r.Pending == nil || r.Pending.Count != 1 || r.Pending.Resources[0] != "ConfigMap/later" {
		t.Fatalf("pending: %+v", r.Pending)
	}
	var pod, rs bool
	for _, o := range r.Observation {
		pod = pod || (o.Ref.Kind == "Pod" && strings.Contains(o.Info, "ImagePullBackOff"))
		rs = rs || o.Ref.Kind == "ReplicaSet"
	}
	if !pod || !rs {
		t.Fatalf("descendants are observations: %+v", r.Observation)
	}
	// Observations are never blockers.
	if hasBlocker(r, RoleWaitingOn, "resource", "Pod", "web-1-x") {
		t.Fatal("a Pod must not be a blocker")
	}
}

func TestHookStates(t *testing.T) {
	mk := func(results string, msg string, phase string) *Report {
		return Explain(snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"`+phase+`","message":"`+msg+`","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},"syncResult":{"revision":"a","resources":[`+results+`]}}}}`))
	}
	t.Run("running hook is identified by hookType", func(t *testing.T) {
		r := mk(`{"group":"batch","kind":"Job","namespace":"web","name":"mig","hookType":"PreSync","hookPhase":"Running","syncPhase":"PreSync"}`,
			"waiting for completion of hook batch/Job/mig", "Running")
		if !hasBlocker(r, RoleWaitingOn, "hook", "Job", "mig") || r.Operation.WaitReason != "hook-completion" {
			t.Fatalf("%+v %s", r.Blockers, r.Operation.WaitReason)
		}
	})
	t.Run("a Succeeded hook is history, not a blocker", func(t *testing.T) {
		r := mk(`{"group":"batch","kind":"Job","namespace":"web","name":"mig","hookType":"PreSync","hookPhase":"Succeeded","syncPhase":"PreSync"}`, "waiting for healthy state of apps/Deployment/web", "Running")
		if hasBlocker(r, RoleWaitingOn, "hook", "Job", "mig") || hasBlocker(r, RoleFailed, "hook", "Job", "mig") {
			t.Fatalf("finished hook must not be a blocker: %+v", r.Blockers)
		}
	})
	t.Run("failed hook", func(t *testing.T) {
		r := mk(`{"group":"batch","kind":"Job","namespace":"web","name":"mig","hookType":"PostSync","hookPhase":"Failed","syncPhase":"PostSync","message":"Job has reached the specified backoff limit"}`,
			"one or more synchronization tasks completed unsuccessfully, reason: x", "Failed")
		if r.Verdict != VerdictFailed || !hasBlocker(r, RoleFailed, "hook", "Job", "mig") {
			t.Fatalf("%s %+v", r.Verdict, r.Blockers)
		}
	})
	t.Run("a non-hook resource with a hookPhase is not a hook", func(t *testing.T) {
		r := mk(`{"group":"apps","kind":"Deployment","namespace":"web","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"Sync"}`, "waiting for healthy state of apps/Deployment/web", "Running")
		b := r.Blockers[0]
		if b.Type != "resource" || b.HookType != "" {
			t.Fatalf("%+v", b)
		}
	})
	t.Run("dry-run failure ends the operation before SyncFail hooks", func(t *testing.T) {
		r := mk(`{"group":"","kind":"ConfigMap","namespace":"web","name":"c","status":"SyncFailed","message":"bad","syncPhase":"Sync"}`, "one or more objects failed to apply (dry run)", "Failed")
		if !hasNote(r, "failed-in-dry-run") || !hasBlocker(r, RoleFailed, "resource", "ConfigMap", "c") {
			t.Fatalf("%+v %+v", r.Notes, r.Blockers)
		}
	})
	t.Run("retries exhausted", func(t *testing.T) {
		r := mk(`{"group":"","kind":"ConfigMap","namespace":"web","name":"c","status":"SyncFailed","message":"bad","syncPhase":"Sync"}`, "x (retried 5 times).", "Failed")
		if !hasNote(r, "retries-exhausted") {
			t.Fatalf("%+v", r.Notes)
		}
	})
	t.Run("prune confirmation named only in the message", func(t *testing.T) {
		r := mk(``, "waiting for pruning confirmation of /ConfigMap/gone", "Running")
		if !hasBlocker(r, RoleWaitingOn, "prune-confirmation", "ConfigMap", "gone") || r.Operation.WaitReason != "prune-confirmation" {
			t.Fatalf("%+v", r.Blockers)
		}
	})
	t.Run("running with nothing recorded is an explicit unknown", func(t *testing.T) {
		r := mk(``, "", "Running")
		if len(r.Blockers) != 0 || !hasUnknownItem(r, "what the operation waits on") {
			t.Fatalf("%+v %+v", r.Blockers, r.Unknowns)
		}
	})
	t.Run("selective sync is noted and pending is not computed", func(t *testing.T) {
		s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync"},"resources":[{"kind":"ConfigMap","name":"x","namespace":"web","status":"OutOfSync"}],
		 "operationState":{"phase":"Running","message":"waiting for healthy state of apps/Deployment/web","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{"resources":[{"kind":"Deployment","name":"web"}]}},"syncResult":{"revision":"a"}}}}`)
		r := Explain(s)
		if !hasNote(r, "selective-sync") || r.Pending != nil {
			t.Fatalf("%+v %+v", r.Notes, r.Pending)
		}
	})
}

func TestHealthIsNeverGuessed(t *testing.T) {
	app := `{` + base + `,"status":{"resourceHealthSource":"appTree","sync":{"status":"OutOfSync"},"health":{"status":"Progressing"},
	 "resources":[{"group":"apps","kind":"Deployment","name":"web","namespace":"web","status":"OutOfSync"}],
	 "operationState":{"phase":"Running","message":"waiting for healthy state of apps/Deployment/web","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},
	 "syncResult":{"revision":"a","resources":[{"group":"apps","kind":"Deployment","namespace":"web","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"Sync"}]}}}}`
	r := Explain(snap(t, "kubeconfig", app))
	h := r.Blockers[0].Health
	if h == nil || h.Status != Unknown || !strings.Contains(h.Message, "Argo CD API") {
		t.Fatalf("health must be unknown without the API: %+v", h)
	}
	if len(r.OutOfSync) != 1 || !strings.HasPrefix(r.OutOfSync[0].Missing, "unknown") {
		t.Fatalf("missing vs modified must be unknown: %+v", r.OutOfSync)
	}
	// With the tree the same fact becomes known.
	s := snap(t, "api", app)
	s.Tree = &model.ResourceTree{Nodes: []model.TreeNode{{NodeRef: model.NodeRef{Group: "apps", Kind: "Deployment", Namespace: "web", Name: "web"}, Health: &model.Health{Status: "Missing"}}}}
	r = Explain(s)
	if r.Blockers[0].Health.Status != "Missing" || !strings.HasPrefix(r.OutOfSync[0].Missing, "yes") {
		t.Fatalf("%+v %+v", r.Blockers[0].Health, r.OutOfSync)
	}
	// A resource whose health inline is absent is "none-assessed", never Healthy.
	s2 := snap(t, "kubeconfig", `{`+base+`,"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"resources":[{"kind":"ConfigMap","name":"c","namespace":"web","status":"Synced"}]}}`)
	if h := healthFor(s2, Ref{Kind: "ConfigMap", Namespace: "web", Name: "c"}); h.Status != "none-assessed" {
		t.Fatalf("%+v", h)
	}
}

func TestAutoSyncAnalysis(t *testing.T) {
	out := func(pol, extra string) string {
		return `{` + `"metadata":{"name":"web","namespace":"argocd"},"spec":{"project":"default","source":{"repoURL":"http://git/x.git","path":"p","targetRevision":"main"},"destination":{"server":"s","namespace":"web"},"syncPolicy":` + pol + `},
		"status":{"sync":{"status":"OutOfSync","revision":"bbb"},"health":{"status":"Healthy"},` + extra + `}}`
	}
	res := `"resources":[{"kind":"ConfigMap","name":"c","namespace":"web","status":"OutOfSync"}]`
	t.Run("absent", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{}`, res)))
		if !verified(r, "not configured") || r.AutoSync.Enabled {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("enabled false", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{"enabled":false}}`, res)))
		if !verified(r, "disabled") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("automated without enabled means enabled", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{}}`, res)))
		if !r.AutoSync.Enabled || !verified(r, "enabled") {
			t.Fatalf("%+v", r.AutoSync)
		}
		if len(r.AutoSync.Possible) == 0 {
			t.Fatal("with nothing verified the possible reasons must be listed separately")
		}
	})
	t.Run("prune-only differences with prune off", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{}}`, `"resources":[{"kind":"ConfigMap","name":"old","namespace":"web","status":"OutOfSync","requiresPruning":true}]`)))
		if !verified(r, "automated prune is off") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("allowEmpty guard", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{"prune":true}}`, `"resources":[{"kind":"ConfigMap","name":"old","namespace":"web","status":"OutOfSync","requiresPruning":true}]`)))
		if !verified(r, "allowEmpty") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	state := func(phase, rev, srcPath string) string {
		return `"operationState":{"phase":"` + phase + `","message":"it broke","startedAt":"2026-10-03T11:00:00Z","finishedAt":"2026-10-03T11:01:00Z","operation":{"sync":{}},
		 "syncResult":{"revision":"` + rev + `","source":{"repoURL":"http://git/x.git","path":"` + srcPath + `","targetRevision":"main"}}}`
	}
	t.Run("same revision and source after a failed sync", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{}}`, res+`,`+state("Failed", "bbb", "p"))))
		if !verified(r, "does not retry it") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("same revision, succeeded, selfHeal off", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{}}`, res+`,`+state("Succeeded", "bbb", "p"))))
		if !verified(r, "selfHeal is off") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("same revision, succeeded, selfHeal on: only a possible reason", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{"selfHeal":true}}`, res+`,`+state("Succeeded", "bbb", "p"))))
		if verified(r, "selfHeal is off") || !possible(r, "self-heal timeout") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("source changed since the last sync", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{}}`, res+`,`+state("Failed", "bbb", "other"))))
		if verified(r, "does not retry it") || !verified(r, "source changed") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("new revision: not provable, listed as possible", func(t *testing.T) {
		r := Explain(snap(t, "api", out(`{"automated":{}}`, res+`,`+state("Failed", "aaa", "p"))))
		if verified(r, "does not retry it") || !possible(r, "differs from the last synced revision") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("managed by an ApplicationSet", func(t *testing.T) {
		s := snap(t, "api", out(`{"automated":{}}`, res))
		s.App.Metadata.OwnerReferences = []model.OwnerRef{{Kind: "ApplicationSet", Name: "set"}}
		r := Explain(s)
		if r.Application.ManagedBy != "ApplicationSet set" || !possible(r, "RollingSync") || !hasNote(r, "managed-by-applicationset") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("multi-source compares sources and revisions", func(t *testing.T) {
		app := `{"metadata":{"name":"web","namespace":"argocd"},"spec":{"project":"default","sources":[{"repoURL":"a","path":"x","targetRevision":"main"},{"repoURL":"b","path":"y","targetRevision":"main"}],"destination":{"server":"s","namespace":"web"},"syncPolicy":{"automated":{}}},
		"status":{"sync":{"status":"OutOfSync","revisions":["r1","r2"]},"health":{"status":"Healthy"},` + res + `,
		"operationState":{"phase":"Failed","message":"it broke","startedAt":"2026-10-03T11:00:00Z","finishedAt":"2026-10-03T11:01:00Z","operation":{"sync":{}},
		"syncResult":{"revision":"r1","revisions":["r1","r2"],"sources":[{"repoURL":"a","path":"x","targetRevision":"main"},{"repoURL":"b","path":"y","targetRevision":"main"}]}}}}`
		r := Explain(snap(t, "api", app))
		if !verified(r, "does not retry it") {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
	t.Run("a Synced application has no auto-sync analysis", func(t *testing.T) {
		r := Explain(snap(t, "api", `{`+base+`,"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}`))
		if r.AutoSync != nil {
			t.Fatalf("%+v", r.AutoSync)
		}
	})
}

func TestSyncWindowsInReport(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"operation":{"sync":{}},"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Running","message":"Sync operation blocked by sync window","startedAt":"2026-10-03T11:59:00Z","operation":{"initiatedBy":{"automated":true},"sync":{}},"syncResult":{"revision":"a"}}}}`)
	s.Project = proj(win("deny", "* * * * *", "1h", nil))
	r := Explain(s)
	if r.SyncWindows == nil || r.SyncWindows.CanSync || !hasBlocker(r, RoleWaitingOn, "sync-window", "SyncWindow", "default") {
		t.Fatalf("%+v %+v", r.SyncWindows, r.Blockers)
	}
	// 3.4 rules are named when the server is 3.4.
	s.ArgoVersion = "v3.4.9"
	if r := Explain(s); !strings.Contains(r.SyncWindows.Rules, "3.4") {
		t.Fatal(r.SyncWindows.Rules)
	}
	// Project unreadable: explicit unknown, never "allowed".
	s.Project = nil
	s.Unavailable["project"] = "permission denied"
	r = Explain(s)
	if r.SyncWindows != nil || !hasUnknownItem(r, "sync windows") {
		t.Fatalf("%+v %+v", r.SyncWindows, r.Unknowns)
	}
}

func TestAppOfApps(t *testing.T) {
	parent := `{"metadata":{"name":"parent","namespace":"argocd"},"spec":{"project":"default","source":{"repoURL":"a","path":"x","targetRevision":"main"},"destination":{"server":"s","namespace":"argocd"}},
	 "status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"resources":[{"group":"argoproj.io","kind":"Application","name":"child","namespace":"argocd","status":"Synced"}]}}`
	child := `{"metadata":{"name":"child","namespace":"argocd"},"spec":{"project":"default","source":{"repoURL":"a","path":"y","targetRevision":"main"},"destination":{"server":"s","namespace":"c"}},
	 "status":{"sync":{"status":"Synced"},"health":{"status":"Degraded"}}}`
	mk := func(health *model.Health, treeMode bool) *Report {
		s := snap(t, "api", parent)
		cs := snap(t, "api", child)
		s.Children["argocd/child"] = cs
		s.App.Status.ResourceHealthSource = "appTree"
		if treeMode {
			n := model.TreeNode{NodeRef: model.NodeRef{Group: "argoproj.io", Kind: "Application", Namespace: "argocd", Name: "child"}, Health: health}
			s.Tree = &model.ResourceTree{Nodes: []model.TreeNode{n}}
		} else {
			s.Mode = "kubeconfig"
			s.Unavailable["tree"] = "needs the Argo CD API"
		}
		return Explain(s)
	}
	t.Run("without an Application health check the parent does not wait", func(t *testing.T) {
		r := mk(nil, true)
		c := r.Children[0]
		if c.Relation != RelNotWaiting || c.Report == nil || c.Report.Verdict != VerdictDegraded {
			t.Fatalf("%+v", c)
		}
		if r.Verdict != VerdictHealthy || r.ExitCode != 2 {
			t.Fatalf("the parent is healthy but the exit code reflects the child: %s %d", r.Verdict, r.ExitCode)
		}
	})
	t.Run("with a health check the child can be waited on", func(t *testing.T) {
		r := mk(&model.Health{Status: "Degraded"}, true)
		if r.Children[0].Relation != RelCanWait {
			t.Fatalf("%+v", r.Children[0])
		}
	})
	t.Run("without the tree the relation is unknown", func(t *testing.T) {
		r := mk(nil, false)
		if r.Children[0].Relation != RelUnknown {
			t.Fatalf("%+v", r.Children[0])
		}
	})
	t.Run("an unreadable child is an explicit unknown and exit 3", func(t *testing.T) {
		s := snap(t, "api", parent)
		s.Unavailable["child:argocd/child"] = "application: permission denied (HTTP 403)"
		s.Tree = &model.ResourceTree{}
		r := Explain(s)
		if !hasUnknownItem(r, "child application argocd/child") || r.ExitCode != 3 {
			t.Fatalf("%+v %d", r.Unknowns, r.ExitCode)
		}
	})
	t.Run("the parent waiting on a child names it as the blocker", func(t *testing.T) {
		s := snap(t, "api", `{"metadata":{"name":"parent","namespace":"argocd"},"spec":{"project":"default","destination":{"namespace":"argocd"}},
		 "status":{"sync":{"status":"OutOfSync"},"health":{"status":"Progressing"},"resources":[{"group":"argoproj.io","kind":"Application","name":"child","namespace":"argocd","status":"Synced"}],
		 "operationState":{"phase":"Running","message":"waiting for healthy state of argoproj.io/Application/child","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},
		 "syncResult":{"revision":"a","resources":[{"group":"argoproj.io","kind":"Application","namespace":"argocd","name":"child","status":"Synced","hookPhase":"Running","syncPhase":"Sync"}]}}}}`)
		s.Children["argocd/child"] = snap(t, "api", child)
		s.Tree = &model.ResourceTree{Nodes: []model.TreeNode{{NodeRef: model.NodeRef{Group: "argoproj.io", Kind: "Application", Namespace: "argocd", Name: "child"}, Health: &model.Health{Status: "Progressing"}}}}
		r := Explain(s)
		if !hasBlocker(r, RoleWaitingOn, "child-application", "Application", "child") || r.Children[0].Relation != RelWaits {
			t.Fatalf("%+v %+v", r.Blockers, r.Children)
		}
	})
	t.Run("cycles and depth are bounded by the collector, the engine tolerates a deep chain", func(t *testing.T) {
		s := snap(t, "api", parent)
		cur := s
		for i := 0; i < 10; i++ {
			c := snap(t, "api", parent)
			cur.Children["argocd/child"] = c
			cur = c
		}
		_ = Explain(s)
	})
}

func TestUnhealthyObservationsWhenNotSyncing(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"Synced"},"health":{"status":"Degraded"},"resources":[{"group":"apps","kind":"Deployment","name":"web","namespace":"web","status":"Synced"}]}}`)
	s.Tree = &model.ResourceTree{Nodes: []model.TreeNode{{NodeRef: model.NodeRef{Group: "apps", Kind: "Deployment", Namespace: "web", Name: "web"}, Health: &model.Health{Status: "Degraded", Message: "ProgressDeadlineExceeded"}}}}
	r := Explain(s)
	if r.Verdict != VerdictDegraded || len(r.Observation) != 1 || r.Observation[0].Health.Message != "ProgressDeadlineExceeded" {
		t.Fatalf("%s %+v", r.Verdict, r.Observation)
	}
	if len(r.Blockers) != 0 {
		t.Fatal("no operation: nothing is blocked")
	}
}

func TestAutoSyncHeldByWindowWithNoOperation(t *testing.T) {
	// A finished manual operation must not make the window check assume a manual sync now.
	s := snap(t, "api", `{`+`"metadata":{"name":"web","namespace":"argocd"},"spec":{"project":"default","destination":{"server":"s","namespace":"web"},"syncPolicy":{"automated":{}}},
	 "status":{"sync":{"status":"OutOfSync","revision":"b"},"health":{"status":"Healthy"},"resources":[{"kind":"ConfigMap","name":"c","namespace":"web","status":"OutOfSync"}],
	 "operationState":{"phase":"Succeeded","startedAt":"2026-10-03T10:00:00Z","finishedAt":"2026-10-03T10:00:05Z","operation":{"initiatedBy":{"username":"alice"},"sync":{}},"syncResult":{"revision":"a"}}}}`)
	s.Project = proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) { w.ManualSync = true }))
	r := Explain(s)
	if r.SyncWindows == nil || r.SyncWindows.CanSync || r.SyncWindows.Manual {
		t.Fatalf("auto-sync must be evaluated as automated even though the last operation was manual: %+v", r.SyncWindows)
	}
	if !verified(r, "sync window blocks automated syncs") {
		t.Fatalf("%+v", r.AutoSync)
	}
	if !strings.Contains(r.Summary, "sync window") {
		t.Fatalf("headline should name the blocking finding: %s", r.Summary)
	}
}

func TestQueuedManualOperationUsesItsInitiatorForWindows(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"operation":{"initiatedBy":{"username":"alice"},"sync":{}},"status":{"sync":{"status":"OutOfSync"}}}`)
	s.Project = proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) { w.ManualSync = true }))
	r := Explain(s)
	if r.SyncWindows == nil || !r.SyncWindows.CanSync || !r.SyncWindows.Manual {
		t.Fatalf("manualSync deny window lets a queued manual operation through: %+v", r.SyncWindows)
	}
}
