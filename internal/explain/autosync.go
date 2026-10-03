package explain

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

func isAutomatedEnabled(sp *model.SyncPolicy) bool {
	return sp != nil && sp.Automated != nil && (sp.Automated.Enabled == nil || *sp.Automated.Enabled)
}

func boolv(p *bool) bool { return p != nil && *p }

// freeForm lists source fields whose content is user data: false, 0 and empty values inside them
// are meaningful (helm valuesObject, kustomize patches) and must never be normalised away.
var freeForm = map[string]bool{"valuesObject": true, "patches": true, "values": true, "parameters": true, "fileParameters": true, "env": true, "args": true}

// normalize drops nulls and empty values of known typed fields so that two decoded sources compare
// the way typed structs with omitempty fields do. Free-form subtrees are kept as they are.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			if freeForm[k] {
				if x != nil {
					out[k] = x
				}
				continue
			}
			n := normalize(x)
			if isEmpty(n) {
				continue
			}
			out[k] = n
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, normalize(x))
		}
		return out
	default:
		return v
	}
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

func sourcesEqual(a, b []model.Source) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(normalize(a[i].Raw), normalize(b[i].Raw)) {
			return false
		}
	}
	return true
}

func specSources(a model.Application) []model.Source {
	if len(a.Spec.Sources) > 0 {
		return a.Spec.Sources
	}
	if a.Spec.Source != nil {
		return []model.Source{*a.Spec.Source}
	}
	return nil
}

func resultSources(sr *model.SyncResult) []model.Source {
	if len(sr.Sources) > 0 {
		return sr.Sources
	}
	if sr.Source != nil {
		return []model.Source{*sr.Source}
	}
	return nil
}

// analyzeAutoSync implements A8: verified constraints versus possible reasons.
// It mirrors controller/appcontroller.go autoSync and alreadyAttemptedSync.
func analyzeAutoSync(s *model.Snapshot, r *Report) {
	a := s.App
	if a.Status.Sync.Status == "Synced" && r.Operation.Kind != OpBackoff {
		return
	}
	info := &AutoSyncInfo{Verified: []Finding{}, Possible: []Finding{}}
	v := func(text, src string) {
		info.Verified = append(info.Verified, Finding{Text: text, Blocks: true, Source: src})
	}
	info0 := func(text, src string) { info.Verified = append(info.Verified, Finding{Text: text, Source: src}) }
	p := func(text, src string) { info.Possible = append(info.Possible, Finding{Text: text, Source: src}) }

	sp := a.Spec.SyncPolicy
	info.Enabled = isAutomatedEnabled(sp)
	switch {
	case sp == nil || sp.Automated == nil:
		v("automated sync is not configured: Argo CD does not start syncs for this Application by itself", ".spec.syncPolicy.automated (absent)")
	case !info.Enabled:
		v("automated sync is configured but disabled (enabled: false)", ".spec.syncPolicy.automated.enabled")
	default:
		info0("automated sync is enabled (an `automated` block without `enabled` means enabled)", ".spec.syncPolicy.automated")
	}

	if a.Metadata.DeletionTimestamp != nil {
		v("the Application is being deleted; auto-sync is skipped", ".metadata.deletionTimestamp")
	}

	switch r.Operation.Kind {
	case OpQueued, OpRunning, OpTerminating:
		v("an operation is already in progress; auto-sync does not start another", ".operation / .status.operationState.phase")
	case OpBackoff:
		st := a.Status.OperationState
		next := r.Operation.NextRetryNotBefore
		lim := "unlimited"
		if !r.Operation.Retry.Unlimited {
			lim = fmt.Sprintf("%d", r.Operation.Retry.Limit)
		}
		v(fmt.Sprintf("the operation is in retry backoff: attempt %d of %s, next attempt not before %s (the earliest time; the controller retries on its next pass after it)",
			st.RetryCount, lim, next), ".status.operationState (phase=Running, finishedAt set), .operation.retry")
	}

	if !info.Enabled || sp == nil {
		r.AutoSync = withPossible(info, s, r)
		return
	}
	if a.Status.Sync.Status != "OutOfSync" {
		v(fmt.Sprintf("sync status is %s: auto-sync only acts on OutOfSync", orUnknown(a.Status.Sync.Status)), ".status.sync.status")
		r.AutoSync = withPossible(info, s, r)
		return
	}

	auto := sp.Automated
	// prune-only guard
	if !boolv(auto.Prune) {
		pruneOnly := true
		for _, rs := range a.Status.Resources {
			if rs.Status != "Synced" && !rs.RequiresPruning {
				pruneOnly = false
				break
			}
		}
		if pruneOnly {
			v("the only differences are resources to prune and automated prune is off: auto-sync is skipped", ".spec.syncPolicy.automated.prune, .status.resources[].requiresPruning")
		}
	}

	// already attempted
	st := a.Status.OperationState
	desired := comparedRevisions(a)
	if st != nil && r.Operation.Kind == OpFinished {
		attempted := false
		var lastRevs []string
		if st.SyncResult == nil {
			attempted = model.Completed(st.Phase)
		} else {
			lastRevs = syncResultRevisions(st.SyncResult)
			revEq := strings.Join(lastRevs, ",") == strings.Join(desired, ",")
			srcEq := sourcesEqual(specSources(a), resultSources(st.SyncResult))
			attempted = revEq && srcEq
			if !revEq {
				p("the latest commit differs from the last synced revision, but the controller may skip the revision comparison when the commit does not touch this Application's path; that decision is not visible in the Application",
					".status.sync.revision vs .status.operationState.syncResult.revision")
			}
			if revEq && !srcEq {
				info0("revisions match but the Application source changed since the last sync: auto-sync would try a new sync", ".spec.source vs .status.operationState.syncResult.source")
			}
		}
		if attempted {
			if st.Phase != model.PhaseSucceeded {
				v(fmt.Sprintf("the last sync attempt to %s with the same revision and source %s and auto-sync does not retry it: %s",
					shortList(lastRevs), strings.ToLower(st.Phase), oneLine(st.Message)),
					".status.operationState (phase, message, syncResult.revision, syncResult.source)")
			} else if !boolv(auto.SelfHeal) {
				v(fmt.Sprintf("the most recent sync already went to %s successfully and selfHeal is off: drift after that sync is not corrected automatically", shortList(lastRevs)),
					".spec.syncPolicy.automated.selfHeal, .status.operationState.syncResult")
			} else {
				p("selfHeal is on and the last sync to this revision succeeded: a new sync waits for the self-heal timeout/backoff, which is an application-controller setting not stored in the Application",
					".spec.syncPolicy.automated.selfHeal")
			}
		}
	}

	// allowEmpty guard
	if boolv(auto.Prune) && !boolv(auto.AllowEmpty) {
		all := true
		for _, rs := range a.Status.Resources {
			if !rs.RequiresPruning {
				all = false
			}
		}
		if all {
			v("every resource would be pruned and allowEmpty is off: auto-sync refuses to wipe out the Application",
				".spec.syncPolicy.automated.{prune,allowEmpty}, .status.resources[].requiresPruning")
		}
	}

	if r.SyncWindows != nil && !r.SyncWindows.CanSync {
		v("a sync window blocks automated syncs now ("+r.SyncWindows.Reason+"): the controller does not start an auto-sync while the windows deny it, and records nothing in the Application", "AppProject .spec.syncWindows evaluated for an automated sync")
	}
	r.AutoSync = withPossible(info, s, r)
}

func withPossible(info *AutoSyncInfo, s *model.Snapshot, r *Report) *AutoSyncInfo {
	a := s.App
	p := func(text, src string) { info.Possible = append(info.Possible, Finding{Text: text, Source: src}) }
	if info.Enabled && a.Status.Sync.Status == "OutOfSync" && !anyBlocks(info) && r.Operation.Kind != OpRunning && r.Operation.Kind != OpQueued {
		if a.Status.ReconciledAt != nil {
			age := s.Now.Sub(*a.Status.ReconciledAt).Round(time.Second)
			p(fmt.Sprintf("no verified constraint found; the controller may simply not have acted yet (last reconciled %s ago), or it is not running or not responsible for this Application", age), ".status.reconciledAt")
		} else {
			p("no verified constraint found; the controller may not have reconciled this Application yet, or is not running", ".status.reconciledAt (absent)")
		}
	}
	for _, o := range a.Metadata.OwnerReferences {
		if o.Kind == "ApplicationSet" {
			p("the Application is managed by ApplicationSet "+o.Name+"; a RollingSync strategy disables auto-sync on its Applications and drives syncs itself", ".metadata.ownerReferences")
		}
	}
	return info
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func anyBlocks(a *AutoSyncInfo) bool {
	for _, f := range a.Verified {
		if f.Blocks {
			return true
		}
	}
	return false
}
