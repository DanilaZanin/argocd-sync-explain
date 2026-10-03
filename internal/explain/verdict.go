package explain

import (
	"fmt"
	"strings"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

// RelCanWait: Argo CD assesses the child's health, so a parent sync can wait on it.
const RelCanWait = "health-assessed"

func isApplicationRef(g, k string) bool { return g == "argoproj.io" && k == "Application" }

// analyzeChildren implements A5. A parent waits for a child Application only
// if Argo CD assesses Application health (a custom check in argocd-cm). The
// relation is derived from the health the parent's resource entry carries.
func analyzeChildren(s *model.Snapshot, r *Report, depth int) {
	for _, rs := range s.App.Status.Resources {
		if !isApplicationRef(rs.Group, rs.Kind) || rs.Hook {
			continue
		}
		ns := rs.Namespace
		if ns == "" {
			ns = s.App.Metadata.Namespace
		}
		ref := refOfStatus(rs)
		ref.Namespace = ns
		lookup := refOfStatus(rs) // as listed in the parent
		ci := ChildInfo{Ref: ref, Source: ".status.resources[] + resource tree"}
		h := healthFor(s, lookup)
		ci.Health = h
		waiting, failedHere := false, false
		for _, b := range r.Blockers {
			if b.Type == "child-application" && sameRef(b.Ref, lookup) {
				waiting = waiting || (b.Role == RoleWaitingOn && r.Operation.Kind == OpRunning)
				failedHere = failedHere || b.Role == RoleFailed
			}
		}
		assessed := h.Status != Unknown && h.Status != "none-assessed"
		switch {
		case waiting && assessed:
			ci.Relation = RelWaits
			ci.Evidence = "the parent's running operation waits on this child Application (task hookPhase=Running) and its health is assessed: " + h.Status
		case waiting:
			ci.Relation = RelUnknown
			ci.Evidence = "the operation reports waiting on this child, but its health assessment is not visible: " + h.Status + " " + h.Message
		case h.Status == Unknown:
			ci.Relation = RelUnknown
			ci.Evidence = "cannot tell whether Argo CD assesses Application health here: " + h.Message
		case h.Status == "none-assessed":
			ci.Relation = RelNotWaiting
			ci.Evidence = "no health is assessed for Application resources on this installation (no health check for argoproj.io/Application in argocd-cm), so the parent's sync does not wait for this child"
		default:
			ci.Relation = RelCanWait
			ci.Evidence = "Argo CD assesses this child's health (" + h.Status + "), so a parent sync with later waves waits for it to be Healthy"
		}
		if failedHere {
			ci.Evidence += "; the parent's operation failed on this child"
		}
		key := ns + "/" + ref.Name
		if cs, ok := s.Children[key]; ok {
			if depth < maxChildDepth {
				ci.Report = explain(cs, depth+1)
			} else {
				r.unknown("child application "+key, fmt.Sprintf("depth limit of %d levels reached; its descendants are not explained", maxChildDepth))
			}
		} else if why, ok := s.Unavailable["child:"+key]; ok {
			r.unknown("child application "+key, why)
		}
		r.Children = append(r.Children, ci)
	}
}

func pickVerdict(s *model.Snapshot, r *Report) {
	a := s.App
	set := func(v, summary string) {
		r.Verdict = v
		r.Summary = summary
	}
	switch {
	case a.Metadata.DeletionTimestamp != nil:
		set(VerdictDeleting, deletingSummary(r))
	}
	switch r.Operation.Kind {
	case OpQueued:
		if r.Verdict != "" {
			break
		}
		set(VerdictQueued, "A sync was requested and the application controller has not started it yet.")
	case OpTerminating:
		if r.Verdict == "" {
			set(VerdictTerminating, "The sync operation is being terminated.")
		}
	case OpRunning:
		switch {
		case r.Verdict != "":
		case len(r.Blockers) == 0:
			set(VerdictUnknown, "The sync operation is running, but nothing it waits on is recorded and the message gives no recognised reason.")
		default:
			set(VerdictWaiting, waitingSummary(r))
		}
	case OpBackoff:
		if r.Verdict != "" {
			break
		}
		set(VerdictRetrying, fmt.Sprintf("The last sync attempt failed; the operation is in retry backoff (attempt %d), next attempt not before %s.",
			r.Operation.RetryCount, orUnknown(r.Operation.NextRetryNotBefore)))
	case OpFinished:
		if r.Verdict == "" && (r.Operation.Phase == model.PhaseFailed || r.Operation.Phase == model.PhaseError) {
			set(VerdictFailed, failedSummary(r))
		}
	}
	if r.Verdict == "" {
		switch {
		case a.Status.Sync.Status == "" && a.Status.Health.Status == "" && len(a.Status.Conditions) == 0:
			set(VerdictUnknown, "The Application has no status yet: Argo CD has not reconciled it.")
		case hasErrorCondition(a):
			set(VerdictError, "Argo CD reports an error condition: "+oneLine(errorCondition(a))+".")
		case a.Status.Sync.Status == "Unknown" || a.Status.Sync.Status == "":
			set(VerdictUnknown, "Sync status is Unknown and no error condition explains it.")
		case a.Status.Sync.Status == "OutOfSync":
			set(VerdictOutOfSync, outOfSyncSummary(r))
		case a.Status.Health.Status == "Healthy":
			set(VerdictHealthy, "Synced and Healthy. Nothing is blocked.")
		case a.Status.Health.Status == "Progressing":
			set(VerdictProgressing, progressingSummary(r))
		case a.Status.Health.Status == "Degraded":
			set(VerdictDegraded, "Synced but Degraded now; the last operation "+lastOpText(r)+" and sync itself is not blocked. See the observations for the resources that are Degraded.")
		case a.Status.Health.Status == "":
			set(VerdictUnknown, "Health status is not set.")
		default:
			set(strings.ToLower(a.Status.Health.Status), "Synced; health is "+a.Status.Health.Status+".")
		}
	}
	r.ExitCode = exitFor(r.Verdict)
	for _, c := range r.Children {
		if c.Report == nil {
			if c.Relation == RelUnknown || hasUnknownFor(r, c.Ref) {
				r.ExitCode = worse(r.ExitCode, 3)
			}
			continue
		}
		r.ExitCode = worse(r.ExitCode, c.Report.ExitCode)
	}
	for _, u := range r.Unknowns {
		if strings.HasPrefix(u.What, "child application ") {
			r.ExitCode = worse(r.ExitCode, 3)
		}
	}
}

func hasUnknownFor(r *Report, ref Ref) bool {
	for _, u := range r.Unknowns {
		if u.What == "child application "+ref.Namespace+"/"+ref.Name {
			return true
		}
	}
	return false
}

func exitFor(v string) int {
	switch v {
	case VerdictHealthy:
		return 0
	case VerdictUnknown:
		return 3
	default:
		return 2
	}
}

func worse(a, b int) int {
	rank := map[int]int{0: 0, 3: 1, 2: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func errorCondition(a model.Application) string {
	for _, c := range a.Status.Conditions {
		if strings.HasSuffix(c.Type, "Error") {
			return c.Type + ": " + c.Message
		}
	}
	return ""
}

func lastOpText(r *Report) string {
	if r.Operation.Kind == OpNone {
		return "has no recorded operation"
	}
	return "ended " + strings.ToLower(r.Operation.Phase)
}

func waitingSummary(r *Report) string {
	var parts []string
	for _, b := range r.Blockers {
		switch b.Type {
		case "sync-window":
			parts = append(parts, "a sync window denies the sync")
		case "hook":
			parts = append(parts, fmt.Sprintf("%s hook %s/%s", b.HookType, b.Ref.Kind, b.Ref.Name))
		case "prune-confirmation":
			parts = append(parts, fmt.Sprintf("pruning confirmation for %s/%s", b.Ref.Kind, b.Ref.Name))
		case "child-application":
			parts = append(parts, fmt.Sprintf("child Application %s", b.Ref.Name))
		default:
			parts = append(parts, fmt.Sprintf("%s/%s", b.Ref.Kind, b.Ref.Name))
		}
	}
	if len(parts) == 0 {
		return "The sync operation is running; what it waits on is not recorded (see unknowns)."
	}
	return "The sync operation is running and waits on: " + strings.Join(parts, ", ") + "."
}

func failedSummary(r *Report) string {
	var parts []string
	for _, b := range r.Blockers {
		if b.Role == RoleFailed {
			parts = append(parts, fmt.Sprintf("%s/%s", b.Ref.Kind, b.Ref.Name))
		}
	}
	msg := oneLine(r.Operation.Message)
	if len(parts) == 0 {
		return "The last sync operation " + strings.ToLower(r.Operation.Phase) + ": " + msg
	}
	return fmt.Sprintf("The last sync operation %s on %s: %s", strings.ToLower(r.Operation.Phase), strings.Join(parts, ", "), msg)
}

func outOfSyncSummary(r *Report) string {
	n := len(r.OutOfSync)
	base := fmt.Sprintf("OutOfSync: %d resource(s) differ from the desired state.", n)
	if r.AutoSync != nil && len(r.AutoSync.Verified) > 0 {
		base += " " + autoHeadline(r.AutoSync)
	}
	return base
}

func autoHeadline(a *AutoSyncInfo) string {
	for _, f := range a.Verified {
		if f.Blocks {
			return "Auto-sync: " + f.Text + "."
		}
	}
	return ""
}

func progressingSummary(r *Report) string {
	if r.Operation.Kind == OpFinished && r.Operation.Phase == model.PhaseSucceeded {
		return "The last sync operation finished successfully; the rollout is still Progressing. That is not a blocked sync: the operation does not hold for the health of the last wave."
	}
	return "Synced, but health is Progressing; no sync operation is running."
}

// analyzeUnhealthy lists non-Healthy managed resources as observations when
// the application health is not Healthy. They are context, not blockers (A6).
func analyzeUnhealthy(s *model.Snapshot, r *Report) {
	if s.App.Status.Health.Status == "Healthy" || s.App.Status.Health.Status == "" {
		return
	}
	known := false
	for _, rs := range s.App.Status.Resources {
		if rs.Hook {
			continue
		}
		ref := refOfStatus(rs)
		h := healthFor(s, ref)
		if h.Status == Unknown {
			continue
		}
		known = true
		if h.Status == "Healthy" || h.Status == "none-assessed" {
			continue
		}
		r.Observation = append(r.Observation, Observation{Under: "application health", Ref: ref, Health: h, Source: h.Source})
		addObservations(s, r, ref)
	}
	if !known && len(s.App.Status.Resources) > 0 {
		r.unknown("resource health", "per-resource health is not stored in the Application (status.resourceHealthSource=appTree); needs the Argo CD API")
	}
}

func deletingSummary(r *Report) string {
	d := r.Deleting
	if d == nil {
		return "The Application is being deleted."
	}
	var held []string
	for _, b := range r.Blockers {
		if b.Role == RoleWaitingOn {
			held = append(held, b.Ref.Kind+"/"+b.Ref.Name)
		}
	}
	msg := "The Application is being deleted (deletionTimestamp " + d.Since + ") but still exists."
	if len(held) > 0 {
		msg += " Held by: " + strings.Join(held, ", ") + "."
	} else {
		msg += " What holds it is not fully visible (see remaining resources and unknowns)."
	}
	return msg
}
