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

const staleApp = `{` + base + `,"status":{"sync":{"status":"OutOfSync","revision":"%s"},"health":{"status":"Progressing"},
	 "resources":[{"kind":"ConfigMap","name":"later","namespace":"web","status":"OutOfSync","syncWave":1},{"group":"apps","kind":"Deployment","name":"web","namespace":"web","status":"Synced"}],
	 "operationState":{"phase":"Running","message":"waiting for healthy state of apps/Deployment/web","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},
	  "syncResult":{"revision":"aaaaaaa","resources":[
	    {"kind":"Namespace","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"PreSync"},
	    {"group":"apps","kind":"Deployment","namespace":"web","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"Sync"}]}}}}`

func staleSnap(t *testing.T, cmpRev string) *model.Snapshot {
	s := snap(t, "api", strings.Replace(staleApp, "%s", cmpRev, 1))
	s.Tree = &model.ResourceTree{Nodes: []model.TreeNode{
		{NodeRef: model.NodeRef{Group: "apps", Kind: "Deployment", Namespace: "web", Name: "web", UID: "d"}, Health: &model.Health{Status: "Progressing", Message: "Waiting for rollout"}},
		{NodeRef: model.NodeRef{Group: "apps", Kind: "ReplicaSet", Namespace: "web", Name: "web-1", UID: "r"}, ParentRefs: []model.NodeRef{{UID: "d"}}, Health: &model.Health{Status: "Progressing"}},
		{NodeRef: model.NodeRef{Kind: "Pod", Namespace: "web", Name: "web-1-x", UID: "p"}, ParentRefs: []model.NodeRef{{UID: "r"}}, Health: &model.Health{Status: "Progressing"},
			Info: []model.InfoItem{{Name: "Status Reason", Value: "ImagePullBackOff"}}},
	}}
	return s
}

func TestRunningWaitsUseControllerMessageAndIgnoreStaleResults(t *testing.T) {
	r := Explain(staleSnap(t, "aaaaaaa"))
	if len(r.Blockers) != 1 || !hasBlocker(r, RoleWaitingOn, "resource", "Deployment", "web") {
		t.Fatalf("exactly the Deployment is the blocker: %+v", r.Blockers)
	}
	if !hasNote(r, "stale-running-result") || hasNote(r, "revision-mismatch") {
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
	if hasBlocker(r, RoleWaitingOn, "resource", "Pod", "web-1-x") {
		t.Fatal("a Pod must not be a blocker")
	}
}

// Sol 3: the waves come from the latest comparison. When the operation runs another revision they prove nothing.
func TestRevisionMismatchMakesWaveAndPendingUnknown(t *testing.T) {
	r := Explain(staleSnap(t, "bbbbbbb"))
	if !hasNote(r, "revision-mismatch") {
		t.Fatalf("%+v", r.Notes)
	}
	if r.Position.Wave != Unknown || !strings.HasPrefix(r.Blockers[0].Wave, Unknown) {
		t.Fatalf("wave must be unknown: %+v %+v", r.Position, r.Blockers[0])
	}
	if r.Pending != nil || !hasUnknownItem(r, "pending tasks") {
		t.Fatalf("pending must be unknown: %+v %+v", r.Pending, r.Unknowns)
	}
	// A symbolic (unresolved) operation revision is not comparable and is not called a mismatch.
	s := staleSnap(t, "bbbbbbb")
	s.App.Status.OperationState.SyncResult.Revision = "main"
	if r := Explain(s); hasNote(r, "revision-mismatch") {
		t.Fatalf("%+v", r.Notes)
	}
	// A changed source also makes the current waves untrusted.
	s = staleSnap(t, "aaaaaaa")
	s.App.Status.OperationState.SyncResult.Source = &model.Source{RepoURL: "x", Raw: map[string]any{"repoURL": "x"}}
	if r := Explain(s); r.Position.Wave != Unknown {
		t.Fatalf("%+v", r.Position)
	}
}

// Opus B1: CreateNamespace=true leaves a Running Namespace result next to a PreSync hook.
func TestSyntheticNamespaceNextToHookIsNotABlocker(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync","revision":"aaaaaaa"},"operationState":{"phase":"Running","message":"waiting for completion of hook batch/Job/slow","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},
	 "syncResult":{"revision":"aaaaaaa","resources":[
	  {"kind":"Namespace","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"PreSync"},
	  {"group":"batch","kind":"Job","namespace":"web","name":"slow","hookType":"PreSync","hookPhase":"Running","syncPhase":"PreSync"}]}}}}`)
	r := Explain(s)
	if len(r.Blockers) != 1 || !hasBlocker(r, RoleWaitingOn, "hook", "Job", "slow") {
		t.Fatalf("only the hook is a blocker: %+v", r.Blockers)
	}
	if !hasNote(r, "stale-running-result") || hasUnknownItem(r, "which tasks the operation waits on") {
		t.Fatalf("%+v %+v", r.Notes, r.Unknowns)
	}
}

// Sol 1 and 2: when the count disagrees an unknown wave proves nothing, and parallel SyncFail tasks stay.
func TestUnprovenTasksAreUnknownAndSyncFailStays(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync","revision":"aaaaaaa"},"operationState":{"phase":"Running","message":"waiting for completion of hook batch/Job/a and 1 more hooks","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},
	 "syncResult":{"revision":"aaaaaaa","resources":[
	  {"group":"batch","kind":"Job","namespace":"web","name":"a","hookType":"Sync","hookPhase":"Running","syncPhase":"Sync"},
	  {"group":"batch","kind":"Job","namespace":"web","name":"b","hookType":"Sync","hookPhase":"Running","syncPhase":"Sync"},
	  {"group":"batch","kind":"Job","namespace":"web","name":"fail-hook","hookType":"SyncFail","hookPhase":"Running","syncPhase":"SyncFail"},
	  {"group":"","kind":"ConfigMap","namespace":"web","name":"broken","status":"SyncFailed","message":"bad","hookPhase":"Failed","syncPhase":"Sync"}]}}}}`)
	s.Project = proj()
	r := Explain(s)
	if !hasBlocker(r, RoleWaitingOn, "hook", "Job", "a") || !hasBlocker(r, RoleWaitingOn, "hook", "Job", "fail-hook") {
		t.Fatalf("named task and the parallel SyncFail hook must stay: %+v", r.Blockers)
	}
	if hasBlocker(r, RoleWaitingOn, "hook", "Job", "b") {
		t.Fatalf("an unnamed task with an unknown wave is not proven: %+v", r.Blockers)
	}
	if !hasUnknownItem(r, "which tasks the operation waits on") {
		t.Fatalf("ambiguity must be unknown: %+v", r.Unknowns)
	}
	if !hasBlocker(r, RoleFailed, "resource", "ConfigMap", "broken") || r.Operation.Kind != OpRunning {
		t.Fatalf("failed results are shown while the operation is Running: %+v", r.Blockers)
	}
}

func TestHookDeletionIsItsOwnReason(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Running","message":"waiting for deletion of hook batch/Job/once","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},"syncResult":{"revision":"a"}}}}`)
	r := Explain(s)
	if r.Operation.WaitReason != "hook-deletion" || !hasBlocker(r, RoleWaitingOn, "hook", "Job", "once") {
		t.Fatalf("%s %+v", r.Operation.WaitReason, r.Blockers)
	}
	if strings.Contains(r.Blockers[0].Why, "pruned") {
		t.Fatalf("a hook deletion is not a pruned resource: %s", r.Blockers[0].Why)
	}
}

func TestRunningWithNothingRecordedIsUnknownExit3(t *testing.T) {
	r := Explain(snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync"},"operationState":{"phase":"Running","message":"","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},"syncResult":{"revision":"a"}}}}`))
	if r.Operation.Kind != OpRunning || r.Verdict != VerdictUnknown || r.ExitCode != 3 {
		t.Fatalf("%s %s %d", r.Operation.Kind, r.Verdict, r.ExitCode)
	}
}

func TestDegradedWordingMakesNoCausalClaim(t *testing.T) {
	r := Explain(snap(t, "api", `{`+base+`,"status":{"sync":{"status":"Synced"},"health":{"status":"Degraded"},"operationState":{"phase":"Succeeded","startedAt":"2026-10-03T11:00:00Z","finishedAt":"2026-10-03T11:01:00Z","operation":{"sync":{}}}}}`))
	if strings.Contains(r.Summary, "not the cause") || !strings.Contains(r.Summary, "Degraded now") {
		t.Fatal(r.Summary)
	}
}

func TestPruneLastIsPending(t *testing.T) {
	s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync","revision":"aaaaaaa"},
	 "resources":[{"group":"apps","kind":"Deployment","name":"web","namespace":"web","status":"OutOfSync"},{"kind":"ConfigMap","name":"old","namespace":"web","status":"OutOfSync","requiresPruning":true}],
	 "operationState":{"phase":"Running","message":"waiting for healthy state of apps/Deployment/web","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{"prune":true}},
	 "syncResult":{"revision":"aaaaaaa","resources":[{"group":"apps","kind":"Deployment","namespace":"web","name":"web","status":"Synced","hookPhase":"Running","syncPhase":"Sync"}]}}}}`)
	r := Explain(s)
	if r.Pending == nil || !contains(strings.Join(r.Pending.Resources, ","), "ConfigMap/old (to prune)") {
		t.Fatalf("%+v", r.Pending)
	}
	s.App.Status.OperationState.Operation.Sync.Prune = false
	if r := Explain(s); r.Pending != nil {
		t.Fatalf("without prune the resource is not pending: %+v", r.Pending)
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

// ---- children (Sol 5, 7) ----

func childParent(t *testing.T, opJSON string) *model.Snapshot {
	s := snap(t, "api", `{"metadata":{"name":"parent","namespace":"argocd"},"spec":{"project":"default","destination":{"server":"https://kubernetes.default.svc","namespace":"argocd"}},
	 "status":{"sync":{"status":"OutOfSync"},"health":{"status":"Progressing"},"resources":[{"group":"argoproj.io","kind":"Application","name":"child","namespace":"argocd","status":"Synced"}],`+opJSON+`}}`)
	s.Children["argocd/child"] = snap(t, "api", `{"metadata":{"name":"child","namespace":"argocd"},"spec":{"project":"default"},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}`)
	return s
}

func TestChildRelationNeedsRunningWaitingOnAndAssessedHealth(t *testing.T) {
	waitOp := `"operationState":{"phase":"Running","message":"waiting for healthy state of argoproj.io/Application/child","startedAt":"2026-10-03T11:59:00Z","operation":{"sync":{}},
	 "syncResult":{"revision":"a","resources":[{"group":"argoproj.io","kind":"Application","namespace":"argocd","name":"child","hookPhase":"Running","syncPhase":"Sync"}]}}`
	failOp := `"operationState":{"phase":"Failed","message":"x","startedAt":"2026-10-03T11:59:00Z","finishedAt":"2026-10-03T11:59:30Z","operation":{"sync":{}},
	 "syncResult":{"revision":"a","resources":[{"group":"argoproj.io","kind":"Application","namespace":"argocd","name":"child","status":"SyncFailed","hookPhase":"Failed","syncPhase":"Sync"}]}}`
	node := func(h *model.Health) *model.ResourceTree {
		return &model.ResourceTree{Nodes: []model.TreeNode{{NodeRef: model.NodeRef{Group: "argoproj.io", Kind: "Application", Namespace: "argocd", Name: "child"}, Health: h}}}
	}
	t.Run("running and assessed: parent-waits", func(t *testing.T) {
		s := childParent(t, waitOp)
		s.Tree = node(&model.Health{Status: "Progressing"})
		if r := Explain(s); r.Children[0].Relation != RelWaits {
			t.Fatalf("%+v", r.Children[0])
		}
	})
	t.Run("waiting but no health assessed is a contradiction, not parent-waits", func(t *testing.T) {
		s := childParent(t, waitOp)
		s.Tree = node(nil)
		if r := Explain(s); r.Children[0].Relation != RelUnknown {
			t.Fatalf("%+v", r.Children[0])
		}
	})
	t.Run("failed parent is not a waiting parent", func(t *testing.T) {
		s := childParent(t, failOp)
		s.Tree = node(&model.Health{Status: "Degraded"})
		r := Explain(s)
		c := r.Children[0]
		if c.Relation == RelWaits || !strings.Contains(c.Evidence, "failed on this child") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("failed parent without Lua keeps parent-does-not-wait", func(t *testing.T) {
		s := childParent(t, failOp)
		s.Tree = node(nil)
		if r := Explain(s); r.Children[0].Relation != RelNotWaiting {
			t.Fatalf("%+v", r.Children[0])
		}
	})
	t.Run("a same-named Application in another namespace is not the waited child", func(t *testing.T) {
		s := childParent(t, waitOp)
		s.App.Status.Resources[0].Namespace = "other"
		s.Tree = &model.ResourceTree{Nodes: []model.TreeNode{{NodeRef: model.NodeRef{Group: "argoproj.io", Kind: "Application", Namespace: "other", Name: "child"}, Health: &model.Health{Status: "Healthy"}}}}
		if r := Explain(s); r.Children[0].Relation == RelWaits {
			t.Fatalf("%+v", r.Children[0])
		}
	})
}

func TestDepthLimitAndUnreadableDescendantsAreUnknownNotHealthy(t *testing.T) {
	mk := func() *model.Snapshot {
		return snap(t, "api", `{"metadata":{"name":"n","namespace":"argocd"},"spec":{"project":"default"},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},
		 "resources":[{"group":"argoproj.io","kind":"Application","name":"n","namespace":"argocd"}]}}`)
	}
	root := mk()
	cur := root
	for i := 0; i < 8; i++ {
		c := mk()
		cur.Children["argocd/n"] = c
		cur = c
	}
	r := Explain(root)
	if r.ExitCode != 3 {
		t.Fatalf("a healthy chain that was cut off must not exit 0: %d", r.ExitCode)
	}
	found := false
	var walk func(*Report)
	walk = func(x *Report) {
		for _, u := range x.Unknowns {
			if strings.Contains(u.Reason, "depth limit") {
				found = true
			}
		}
		for _, c := range x.Children {
			if c.Report != nil {
				walk(c.Report)
			}
		}
	}
	walk(r)
	if !found {
		t.Fatal("the depth limit must be reported as unknown")
	}
	// The collector reports its own limit and cycles through Unavailable; the engine turns them into unknowns.
	s := mk()
	s.Project = proj()
	s.Unavailable["child:argocd/n"] = "depth limit of 5 levels reached; this child and its descendants are not read"
	if r := Explain(s); r.ExitCode != 3 || !hasUnknownItem(r, "child application argocd/n") || len(r.Unknowns) != 1 {
		t.Fatalf("one unknown per child, exit 3: %+v %d", r.Unknowns, r.ExitCode)
	}
}

// ---- deleting (Opus B2) ----

func TestDeletingApplication(t *testing.T) {
	app := `{"metadata":{"name":"web","namespace":"argocd","deletionTimestamp":"2026-10-03T11:00:00Z","finalizers":["resources-finalizer.argocd.argoproj.io"]},
	 "spec":{"project":"default","destination":{"server":"s","namespace":"web"}},
	 "status":{"sync":{"status":"Synced"},"health":{"status":"Progressing"},"resources":[{"kind":"ConfigMap","name":"held","namespace":"web","status":"Synced"},{"kind":"ConfigMap","name":"gone","namespace":"web","status":"Synced"}]}}`
	t.Run("a resource held by a finalizer is the blocker", func(t *testing.T) {
		s := snap(t, "api", app)
		s.Tree = &model.ResourceTree{Nodes: []model.TreeNode{{NodeRef: model.NodeRef{Kind: "ConfigMap", Namespace: "web", Name: "held"}}}}
		ts := time.Date(2026, 10, 3, 11, 0, 1, 0, time.UTC)
		s.Live = map[string]*model.LiveObject{
			"/ConfigMap/web/held": {Exists: true, DeletionTimestamp: &ts, Finalizers: []string{"ase/hold"}},
			"/ConfigMap/web/gone": {Exists: false},
		}
		r := Explain(s)
		if r.Verdict != VerdictDeleting || r.ExitCode != 2 {
			t.Fatalf("%s %d: %s", r.Verdict, r.ExitCode, r.Summary)
		}
		if !hasBlocker(r, RoleWaitingOn, "resource", "ConfigMap", "held") || hasBlocker(r, RoleWaitingOn, "resource", "ConfigMap", "gone") {
			t.Fatalf("%+v", r.Blockers)
		}
		if r.Deleting == nil || !strings.HasPrefix(r.Deleting.Cascade, "cascade") || len(r.Deleting.Remaining) != 1 || r.Deleting.Remaining[0].Finalizers[0] != "ase/hold" {
			t.Fatalf("%+v", r.Deleting)
		}
	})
	t.Run("non-cascade", func(t *testing.T) {
		s := snap(t, "api", strings.Replace(app, `"resources-finalizer.argocd.argoproj.io"`, `"example.com/other"`, 1))
		s.Tree = &model.ResourceTree{}
		r := Explain(s)
		if r.Verdict != VerdictDeleting || !strings.HasPrefix(r.Deleting.Cascade, "non-cascade") || !hasNote(r, "other-finalizers") {
			t.Fatalf("%+v", r.Deleting)
		}
	})
	t.Run("kubeconfig mode cannot see what remains", func(t *testing.T) {
		s := snap(t, "kubeconfig", app)
		s.Unavailable["live"] = "needs the Argo CD API"
		r := Explain(s)
		if r.Verdict != VerdictDeleting || !hasUnknownItem(r, "remaining resources") {
			t.Fatalf("%s %+v", r.Verdict, r.Unknowns)
		}
	})
	t.Run("a healthy snapshot with a deletionTimestamp is not healthy", func(t *testing.T) {
		s := snap(t, "api", strings.Replace(app, `"health":{"status":"Progressing"}`, `"health":{"status":"Healthy"}`, 1))
		s.Tree = &model.ResourceTree{}
		if r := Explain(s); r.Verdict != VerdictDeleting || r.ExitCode == 0 {
			t.Fatalf("%s %d", r.Verdict, r.ExitCode)
		}
	})
}

// ---- auto-sync details (Sol 8, 9) ----

func TestValuesObjectFalseAndZeroAreNotNormalisedAway(t *testing.T) {
	mk := func(vals string) []model.Source {
		var src model.Source
		if err := json.Unmarshal([]byte(`{"repoURL":"r","path":"p","helm":{"valuesObject":`+vals+`}}`), &src); err != nil {
			t.Fatal(err)
		}
		return []model.Source{src}
	}
	if sourcesEqual(mk(`{"replicaCount":0}`), mk(`{}`)) || sourcesEqual(mk(`{"debug":false}`), mk(`{}`)) {
		t.Fatal("false/0 inside valuesObject are values, not absent")
	}
	if !sourcesEqual(mk(`{"replicaCount":0}`), mk(`{"replicaCount":0}`)) {
		t.Fatal("identical values are equal")
	}
	// typed omitempty fields are still normalised
	var a, b model.Source
	_ = json.Unmarshal([]byte(`{"repoURL":"r","directory":{"recurse":false}}`), &a)
	_ = json.Unmarshal([]byte(`{"repoURL":"r"}`), &b)
	if !sourcesEqual([]model.Source{a}, []model.Source{b}) {
		t.Fatal("typed empty fields compare equal like omitempty structs")
	}
}

func TestSourceChangedFindingDoesNotBlock(t *testing.T) {
	app := `{"metadata":{"name":"web","namespace":"argocd"},"spec":{"project":"default","source":{"repoURL":"http://git/x.git","path":"new","targetRevision":"main"},"destination":{"server":"s","namespace":"web"},"syncPolicy":{"automated":{}}},
	 "status":{"sync":{"status":"OutOfSync","revision":"bbb"},"health":{"status":"Healthy"},"resources":[{"kind":"ConfigMap","name":"c","namespace":"web","status":"OutOfSync"}],
	 "operationState":{"phase":"Failed","message":"x","startedAt":"2026-10-03T11:00:00Z","finishedAt":"2026-10-03T11:01:00Z","operation":{"sync":{}},"syncResult":{"revision":"bbb","source":{"repoURL":"http://git/x.git","path":"old","targetRevision":"main"}}}}}`
	r := Explain(snap(t, "api", app))
	for _, f := range r.AutoSync.Verified {
		if strings.Contains(f.Text, "source changed") && f.Blocks {
			t.Fatalf("a permissive finding must not be marked as blocking: %+v", f)
		}
	}
	if !verified(r, "source changed") {
		t.Fatalf("%+v", r.AutoSync)
	}
	if len(r.AutoSync.Possible) == 0 {
		t.Fatal("with nothing blocking, the possible reasons must be listed")
	}
}

// ---- sync windows (Sol 12, 13) ----

func TestSyncOverrunUsesTheCurrentZoneOffsetLikeArgo353(t *testing.T) {
	app := model.Application{}
	app.Metadata.Name = "web"
	p := proj(win("deny", "0 12 * * *", "1h", func(w *model.SyncWindow) { w.SyncOverrun = true; w.TimeZone = "America/New_York" }))
	// now is in summer (EDT, UTC-4): 2026-07-01 16:30Z is 12:30 local, inside the deny window.
	now := time.Date(2026, 7, 1, 16, 30, 0, 0, time.UTC)
	// The operation started in winter at 16:30Z. With the offset of the check time (-4h) that is 12:30
	// local, inside the deny window, so syncOverrun does not apply. With the winter offset (-5h) it
	// would be 11:30 and overrun would wrongly be allowed.
	start := time.Date(2026, 1, 15, 16, 30, 0, 0, time.UTC)
	ev := EvalWindows(p, app, now, false, &start, WindowRules{SyncOverrun: true})
	if ev.CanSync || ev.OverrunApplied {
		t.Fatalf("%+v", ev)
	}
}

func TestTimeZonesComeFromEmbeddedData(t *testing.T) {
	// Pacific/Kiritimati (UTC+14) exists in the embedded database regardless of the host.
	app := model.Application{}
	app.Metadata.Name = "web"
	p := proj(win("deny", "0 6 * * *", "1h", func(w *model.SyncWindow) { w.TimeZone = "Pacific/Kiritimati" }))
	now := time.Date(2026, 10, 3, 16, 30, 0, 0, time.UTC) // 06:30 on Kiritimati
	if ev := EvalWindows(p, app, now, false, nil, WindowRules{}); ev.CanSync {
		t.Fatalf("a valid zone must be honoured, not replaced by UTC: %+v", ev)
	}
	p2 := proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) { w.TimeZone = "Not/AZone" }))
	s := snap(t, "api", `{`+base+`,"status":{"sync":{"status":"OutOfSync"}}}`)
	s.Project = p2
	if r := Explain(s); !hasNote(r, "unknown-time-zone") {
		t.Fatalf("an invalid zone name must be called out: %+v", r.Notes)
	}
}
