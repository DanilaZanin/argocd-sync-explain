package explain

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

const maxChildDepth = 5

// Explain builds the report for a snapshot and, recursively, for its children.
func Explain(s *model.Snapshot) *Report {
	return explain(s, 0)
}

func explain(s *model.Snapshot, depth int) *Report {
	a := s.App
	r := &Report{
		Mode:        s.Mode,
		ArgoVersion: s.ArgoVersion,
		Application: AppInfo{
			Name:      a.Metadata.Name,
			Namespace: a.Metadata.Namespace,
			Project:   a.Spec.Project,
			Sync:      orUnknown(a.Status.Sync.Status),
			Health:    orUnknown(a.Status.Health.Status),
			Revisions: comparedRevisions(a),
			Source:    sourceString(a),
		},
	}
	for _, o := range a.Metadata.OwnerReferences {
		if o.Kind == "ApplicationSet" {
			r.Application.ManagedBy = "ApplicationSet " + o.Name
			r.Notes = append(r.Notes, Note{
				Code:   "managed-by-applicationset",
				Text:   fmt.Sprintf("managed by ApplicationSet %s; a RollingSync strategy can hold auto-sync (ApplicationSet explain is not part of v0.1)", o.Name),
				Source: ".metadata.ownerReferences",
			})
		}
	}
	for k, v := range s.Unavailable {
		r.Unknowns = append(r.Unknowns, UnknownItem{What: k, Reason: v})
	}

	r.Operation = classify(s, r)
	switch r.Operation.Kind {
	case OpQueued:
		analyzeQueued(s, r)
	case OpRunning:
		analyzeRunning(s, r)
	case OpBackoff:
		analyzeBackoff(s, r)
	case OpTerminating:
		analyzeRunning(s, r)
	case OpFinished:
		analyzeFinished(s, r)
	}

	analyzeConditions(s, r)
	analyzeOutOfSync(s, r)
	analyzeWindows(s, r)
	if r.Operation.Kind == OpNone || r.Operation.Kind == OpFinished || r.Operation.Kind == OpQueued ||
		r.Operation.Kind == OpBackoff || r.Operation.Kind == OpRunning {
		analyzeAutoSync(s, r)
	}
	analyzeEvents(s, r)
	analyzeChildren(s, r, depth)
	if r.Operation.Kind != OpRunning && r.Operation.Kind != OpBackoff {
		analyzeUnhealthy(s, r)
	}
	pickVerdict(s, r)
	r.sortUnknowns()
	return r
}

func (r *Report) sortUnknowns() {
	sort.SliceStable(r.Unknowns, func(i, j int) bool { return r.Unknowns[i].What < r.Unknowns[j].What })
}

func (r *Report) unknown(what, reason string) {
	for _, u := range r.Unknowns {
		if u.What == what {
			return
		}
	}
	r.Unknowns = append(r.Unknowns, UnknownItem{What: what, Reason: reason})
}

func orUnknown(s string) string {
	if s == "" {
		return Unknown
	}
	return s
}

func comparedRevisions(a model.Application) []string {
	if len(a.Status.Sync.Revisions) > 0 {
		return a.Status.Sync.Revisions
	}
	if a.Status.Sync.Revision != "" {
		return []string{a.Status.Sync.Revision}
	}
	return nil
}

func sourceString(a model.Application) string {
	srcs := a.Spec.Sources
	if len(srcs) == 0 && a.Spec.Source != nil {
		srcs = []model.Source{*a.Spec.Source}
	}
	var out []string
	for _, s := range srcs {
		loc := s.Path
		if s.Chart != "" {
			loc = s.Chart
		}
		out = append(out, fmt.Sprintf("%s %s @ %s", s.RepoURL, loc, s.TargetRevision))
	}
	return strings.Join(out, "; ")
}

func short(rev string) string {
	if len(rev) > 8 && !strings.ContainsAny(rev, "./") {
		return rev[:8]
	}
	return rev
}

func shortList(revs []string) string {
	var out []string
	for _, r := range revs {
		out = append(out, short(r))
	}
	return strings.Join(out, ",")
}

// ---- operation classification (A9) ----

func classify(s *model.Snapshot, r *Report) OperationInfo {
	a := s.App
	st := a.Status.OperationState
	info := OperationInfo{Kind: OpNone, Sources: []string{}}
	switch {
	case a.Operation != nil && (st == nil || model.Completed(st.Phase)):
		info.Kind = OpQueued
		info.Sources = []string{".operation", ".status.operationState"}
		if a.Operation.InitiatedBy.Automated {
			info.Initiator = "automated sync"
		} else {
			info.Initiator = "manual"
		}
		if st != nil {
			// operationState is the previous operation until the controller picks the new one up.
			info.Phase = st.Phase
		}
		return info
	case st == nil:
		return info
	}
	info.Phase = st.Phase
	info.Message = st.Message
	info.RetryCount = st.RetryCount
	info.StartedAt = fmtTime(&st.StartedAt)
	info.FinishedAt = fmtTime(st.FinishedAt)
	if st.Operation.InitiatedBy.Automated {
		info.Initiator = "automated sync"
	} else if st.Operation.InitiatedBy.Username != "" {
		info.Initiator = "manual (" + st.Operation.InitiatedBy.Username + ")"
	} else {
		info.Initiator = "manual"
	}
	if st.SyncResult != nil {
		info.Revisions = syncResultRevisions(st.SyncResult)
	}
	info.Retry = retryInfo(st.Operation.Retry)
	switch {
	case st.Phase == model.PhaseTerminating:
		info.Kind = OpTerminating
		info.Sources = []string{".status.operationState.phase"}
	case st.Phase == model.PhaseRunning && st.FinishedAt != nil:
		info.Kind = OpBackoff
		info.Sources = []string{".status.operationState.phase", ".status.operationState.finishedAt", ".status.operationState.operation.retry"}
		if next, err := nextRetryAt(st.Operation.Retry, st.FinishedAt.UTC(), st.RetryCount); err == nil {
			info.NextRetryNotBefore = next.UTC().Format(time.RFC3339)
		} else {
			r.unknown("next retry time", err.Error())
		}
	case st.Phase == model.PhaseRunning:
		info.Kind = OpRunning
		info.Sources = []string{".status.operationState.phase", ".status.operationState.message", ".status.operationState.syncResult.resources"}
	default:
		info.Kind = OpFinished
		info.Sources = []string{".status.operationState.phase", ".status.operationState.message"}
	}
	return info
}

func syncResultRevisions(sr *model.SyncResult) []string {
	if len(sr.Revisions) > 0 {
		return sr.Revisions
	}
	if sr.Revision != "" {
		return []string{sr.Revision}
	}
	return nil
}

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Defaults from pkg/apis/application/v1alpha1/application_defaults.go.
const (
	defaultRetryDuration    = 5 * time.Second
	defaultRetryMaxDuration = 180 * time.Second
	defaultRetryFactor      = int64(2)
)

func parseDur(s string) (time.Duration, error) {
	if v, err := strconv.Atoi(s); err == nil {
		return time.Duration(v) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("unable to parse %s as a duration", s)
	}
	return d, nil
}

// nextRetryAt mirrors RetryStrategy.NextRetryAt.
func nextRetryAt(rs model.RetryStrategy, last time.Time, retryCount int64) (time.Time, error) {
	maxD, dur, factor := defaultRetryMaxDuration, defaultRetryDuration, defaultRetryFactor
	var err error
	if rs.Backoff != nil {
		if rs.Backoff.Duration != "" {
			if dur, err = parseDur(rs.Backoff.Duration); err != nil {
				return time.Time{}, err
			}
		}
		if rs.Backoff.MaxDuration != "" {
			if maxD, err = parseDur(rs.Backoff.MaxDuration); err != nil {
				return time.Time{}, err
			}
		}
		if rs.Backoff.Factor != nil {
			factor = *rs.Backoff.Factor
		}
	}
	wait := float64(dur) * math.Pow(float64(factor), float64(retryCount))
	if maxD > 0 {
		wait = math.Min(float64(maxD), wait)
	}
	return last.Add(time.Duration(wait)), nil
}

func retryInfo(rs model.RetryStrategy) *RetryInfo {
	ri := &RetryInfo{Limit: rs.Limit, Unlimited: rs.Limit < 0, Source: ".status.operationState.operation.retry"}
	d, f, m := defaultRetryDuration.String(), defaultRetryFactor, defaultRetryMaxDuration.String()
	if rs.Backoff != nil {
		if rs.Backoff.Duration != "" {
			d = rs.Backoff.Duration
		}
		if rs.Backoff.Factor != nil {
			f = *rs.Backoff.Factor
		}
		if rs.Backoff.MaxDuration != "" {
			m = rs.Backoff.MaxDuration
		}
	}
	ri.Backoff = fmt.Sprintf("duration=%s factor=%d maxDuration=%s", d, f, m)
	return ri
}

// ---- lookups ----

func refOfResult(x model.ResourceResult) Ref {
	return Ref{Group: x.Group, Kind: x.Kind, Namespace: x.Namespace, Name: x.Name}
}

func refOfStatus(x model.ResourceStatus) Ref {
	return Ref{Group: x.Group, Kind: x.Kind, Namespace: x.Namespace, Name: x.Name}
}

func sameRef(a, b Ref) bool {
	return a.Group == b.Group && a.Kind == b.Kind && a.Name == b.Name && a.Namespace == b.Namespace
}

func findStatus(a model.Application, ref Ref) *model.ResourceStatus {
	for i := range a.Status.Resources {
		if sameRef(refOfStatus(a.Status.Resources[i]), ref) {
			return &a.Status.Resources[i]
		}
	}
	return nil
}

func findNode(t *model.ResourceTree, ref Ref) *model.TreeNode {
	if t == nil {
		return nil
	}
	for i := range t.Nodes {
		n := &t.Nodes[i]
		if n.Group == ref.Group && n.Kind == ref.Kind && n.Name == ref.Name && n.Namespace == ref.Namespace {
			return n
		}
	}
	return nil
}

// healthFor returns the health of a managed resource with its provenance.
// It never turns a missing value into Healthy.
func healthFor(s *model.Snapshot, ref Ref) *Health {
	if s.Tree != nil {
		if n := findNode(s.Tree, ref); n != nil {
			if n.Health == nil {
				return &Health{Status: "none-assessed", Source: "resource tree node (no health assessed for this kind)"}
			}
			return &Health{Status: n.Health.Status, Message: n.Health.Message, Source: "resource tree node health"}
		}
	}
	if s.App.Status.ResourceHealthSource != "appTree" {
		if rs := findStatus(s.App, ref); rs != nil {
			if rs.Health == nil {
				return &Health{Status: "none-assessed", Source: ".status.resources[].health (absent)"}
			}
			return &Health{Status: rs.Health.Status, Message: rs.Health.Message, Source: ".status.resources[].health"}
		}
	}
	reason := "needs the Argo CD API (status.resourceHealthSource=appTree: health is not stored in the Application)"
	if s.Mode == "api" {
		if why, ok := s.Unavailable["tree"]; ok {
			reason = "resource tree unavailable: " + why
		} else {
			reason = "resource is not in the resource tree"
		}
	} else if s.App.Status.ResourceHealthSource != "appTree" {
		reason = "resource not listed in .status.resources"
	}
	return &Health{Status: Unknown, Message: reason, Source: "-"}
}

func waveFor(a model.Application, ref Ref, hook bool) string {
	if rs := findStatus(a, ref); rs != nil && !rs.Hook {
		return strconv.FormatInt(rs.SyncWave, 10)
	}
	if hook {
		return Unknown + " (hook waves are annotations on the manifest, not recorded in the Application)"
	}
	return Unknown + " (not listed in .status.resources)"
}

// ---- running / waiting (A3) ----

var waitMsg = regexp.MustCompile(`^waiting for (completion of hook|healthy state of|pruning confirmation of|deletion of hook|deletion of) (\S*)/(\S+)/(\S+)(?: and (\d+) more (resources|hooks))?$`)

func parseWait(msg string) (reason string, ref Ref, more int, ok bool) {
	m := waitMsg.FindStringSubmatch(msg)
	if m == nil {
		return "", Ref{}, 0, false
	}
	more, _ = strconv.Atoi(m[5])
	return m[1], Ref{Group: m[2], Kind: m[3], Name: m[4]}, more, true
}

func analyzeRunning(s *model.Snapshot, r *Report) {
	st := s.App.Status.OperationState
	r.Operation.WaitReason = ""

	if strings.HasPrefix(st.Message, "Sync operation blocked by sync window") {
		r.Operation.WaitReason = "sync-window"
		r.Blockers = append(r.Blockers, Blocker{
			Role: RoleWaitingOn, Type: "sync-window", Ref: Ref{Kind: "SyncWindow", Name: s.App.Spec.Project},
			ID: "sync-window", Wave: "-", Message: st.Message,
			Why:    "the operation exists but the controller refuses to run it while sync windows deny it (details in syncWindows)",
			Source: ".status.operationState.message",
		})
	}
	reason, wref, more, parsed := parseWait(st.Message)
	if parsed {
		switch reason {
		case "pruning confirmation of":
			r.Operation.WaitReason = "prune-confirmation"
		case "completion of hook":
			r.Operation.WaitReason = "hook-completion"
		case "healthy state of":
			r.Operation.WaitReason = "resource-health"
		default:
			r.Operation.WaitReason = "deletion"
		}
		_ = more
	}

	var results []model.ResourceResult
	if st.SyncResult != nil {
		results = st.SyncResult.Resources
	}
	waitingRefs := map[string]bool{}
	for _, x := range currentTasks(s, r, results, parsed, wref, more) {
		ref := refOfResult(x)
		isHook := x.HookType != ""
		b := Blocker{
			Role: RoleWaitingOn, Ref: ref, ID: ref.ID(), SyncPhase: x.SyncPhase,
			HookType: x.HookType, HookPhase: x.HookPhase, Result: x.Status,
			Wave: waveFor(s.App, ref, isHook), Message: x.Message,
			Source: ".status.operationState.syncResult.resources",
		}
		switch {
		case isHook:
			b.Type = "hook"
			b.Why = fmt.Sprintf("%s hook has not finished; the operation does not start later tasks until it completes", x.HookType)
			b.Health = healthFor(s, ref)
		case ref.Kind == "Application" && ref.Group == "argoproj.io":
			b.Type = "child-application"
			b.Health = healthFor(s, ref)
			b.Why = "the operation applied this child Application and waits for its health to become Healthy before starting later tasks"
		default:
			b.Type = "resource"
			b.Health = healthFor(s, ref)
			b.Why = "applied by the operation; Argo CD now waits for its health to become Healthy before starting later waves or phases"
		}
		if r.Operation.WaitReason == "prune-confirmation" && parsed && wref.Kind == ref.Kind && wref.Name == ref.Name {
			b.Type = "prune-confirmation"
			b.Why = "pruning this resource needs confirmation (Prune=confirm)"
		}
		waitingRefs[ref.ID()] = true
		r.Blockers = append(r.Blockers, b)
		addObservations(s, r, ref)
	}
	// A message-only wait that the results do not show (prune confirmation or deletion of a pruned resource).
	if parsed && !waitingRefs[wref.ID()] {
		matched := false
		for _, b := range r.Blockers {
			if b.Ref.Kind == wref.Kind && b.Ref.Name == wref.Name && b.Ref.Group == wref.Group {
				matched = true
			}
		}
		if !matched {
			b := Blocker{Role: RoleWaitingOn, Ref: wref, ID: wref.ID(), Wave: Unknown, Message: st.Message,
				Source: ".status.operationState.message"}
			switch r.Operation.WaitReason {
			case "prune-confirmation":
				b.Type = "prune-confirmation"
				b.Why = "the controller reports it waits for confirmation to prune this resource (Prune=confirm)"
			case "deletion":
				b.Type = "resource"
				b.Why = "the controller waits for this pruned resource to finish deleting"
			default:
				b.Type = "resource"
				b.Why = "named by the controller in the operation message"
			}
			r.Blockers = append(r.Blockers, b)
		}
	}
	if len(r.Blockers) == 0 {
		r.unknown("what the operation waits on",
			"operation is Running but no task with hookPhase=Running is recorded and the message does not name one (it may have just started or be in the dry-run)")
	}
	buildPosition(s, r, results)
	buildPending(s, r, results)
	opNotes(s, r)
}

func buildPosition(s *model.Snapshot, r *Report, results []model.ResourceResult) {
	phases := map[string]bool{}
	waves := map[string]bool{}
	for _, b := range r.Blockers {
		if b.SyncPhase != "" {
			phases[b.SyncPhase] = true
		}
		if b.Wave != "-" {
			waves[strings.SplitN(b.Wave, " ", 2)[0]] = true
		}
	}
	pos := &Position{Phase: Unknown, Wave: Unknown,
		Basis: "reconstructed from .status.operationState.syncResult.resources (syncPhase of tasks with hookPhase=Running) and .status.resources[].syncWave; Argo CD publishes no phase/wave cursor"}
	if len(phases) > 0 {
		pos.Phase = joinKeys(phases)
	}
	if len(waves) > 0 {
		pos.Wave = joinKeys(waves)
	}
	if len(results) == 0 {
		pos.Basis += "; no results recorded yet"
	}
	r.Position = pos
}

func joinKeys(m map[string]bool) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func buildPending(s *model.Snapshot, r *Report, results []model.ResourceResult) {
	op := s.App.Status.OperationState.Operation.Sync
	if op != nil && len(op.Resources) > 0 {
		return // selective sync, noted in opNotes
	}
	seen := map[string]bool{}
	for _, x := range results {
		seen[refOfResult(x).ID()] = true
	}
	outOfSyncOnly := false
	if op != nil {
		for _, o := range op.SyncOptions {
			outOfSyncOnly = outOfSyncOnly || o == "ApplyOutOfSyncOnly=true"
		}
	}
	pi := &PendingInfo{Basis: ".status.resources minus .status.operationState.syncResult.resources",
		Caveat: "resources without a result yet; prune tasks run in reverse wave order, PruneLast adds a final wave, and hooks are not listed with waves, so this is a reconstruction, not the controller's task list",
		Waves:  []int64{}, Resources: []string{}}
	waveSet := map[int64]bool{}
	for _, rs := range s.App.Status.Resources {
		ref := refOfStatus(rs)
		if rs.Hook || rs.RequiresPruning || seen[ref.ID()] || (outOfSyncOnly && rs.Status == "Synced") {
			continue
		}
		pi.Resources = append(pi.Resources, ref.Kind+"/"+ref.Name)
		waveSet[rs.SyncWave] = true
	}
	pi.Count = len(pi.Resources)
	for w := range waveSet {
		pi.Waves = append(pi.Waves, w)
	}
	sort.Slice(pi.Waves, func(i, j int) bool { return pi.Waves[i] < pi.Waves[j] })
	sort.Strings(pi.Resources)
	if pi.Count > 0 {
		r.Pending = pi
	}
}

func opNotes(s *model.Snapshot, r *Report) {
	st := s.App.Status.OperationState
	if st == nil {
		return
	}
	if op := st.Operation.Sync; op != nil {
		if len(op.Resources) > 0 {
			r.Notes = append(r.Notes, Note{Code: "selective-sync", Text: "selective sync: only the listed resources are in this operation and hooks are skipped; pending tasks are not computed",
				Source: ".status.operationState.operation.sync.resources"})
		}
		if op.DryRun {
			r.Notes = append(r.Notes, Note{Code: "dry-run", Text: "the operation is a dry run", Source: ".status.operationState.operation.sync.dryRun"})
		}
	}
	// revision mismatch (A2)
	if st.SyncResult != nil {
		opRevs := syncResultRevisions(st.SyncResult)
		cmp := comparedRevisions(s.App)
		if len(opRevs) > 0 && len(cmp) > 0 && strings.Join(opRevs, ",") != strings.Join(cmp, ",") {
			r.Notes = append(r.Notes, Note{Code: "revision-mismatch",
				Text:   fmt.Sprintf("the operation synced revision %s, the latest comparison is against %s", shortList(opRevs), shortList(cmp)),
				Source: ".status.operationState.syncResult.revision vs .status.sync.revision"})
		}
	}
}

// ---- observations (A6) ----

func addObservations(s *model.Snapshot, r *Report, root Ref) {
	if s.Tree == nil {
		return
	}
	rootNode := findNode(s.Tree, root)
	if rootNode == nil {
		return
	}
	under := root.Kind + "/" + root.Name
	// breadth-first over descendants via parentRefs (uid match).
	frontier := map[string]bool{rootNode.UID: true}
	seen := map[string]bool{rootNode.UID: true}
	for len(frontier) > 0 {
		next := map[string]bool{}
		for i := range s.Tree.Nodes {
			n := &s.Tree.Nodes[i]
			if seen[n.UID] {
				continue
			}
			for _, p := range n.ParentRefs {
				if frontier[p.UID] {
					seen[n.UID] = true
					next[n.UID] = true
					o := Observation{Under: under, Ref: Ref{Group: n.Group, Kind: n.Kind, Namespace: n.Namespace, Name: n.Name},
						Source: "resource tree node"}
					if n.Health != nil {
						o.Health = &Health{Status: n.Health.Status, Message: n.Health.Message, Source: "resource tree node health"}
					}
					for _, it := range n.Info {
						if it.Name == "Status Reason" || it.Name == "Containers" || it.Name == "Restart Count" {
							o.Info += it.Name + "=" + it.Value + " "
						}
					}
					o.Info = strings.TrimSpace(o.Info)
					if o.Health != nil && o.Health.Status != "Healthy" || o.Info != "" && n.Kind == "Pod" {
						r.Observation = append(r.Observation, o)
					}
					break
				}
			}
		}
		frontier = next
	}
}

// ---- backoff, failed, finished ----

func analyzeBackoff(s *model.Snapshot, r *Report) {
	st := s.App.Status.OperationState
	collectFailures(s, r)
	opNotes(s, r)
	_ = st
}

func collectFailures(s *model.Snapshot, r *Report) {
	st := s.App.Status.OperationState
	if st.SyncResult == nil {
		return
	}
	for _, x := range st.SyncResult.Resources {
		failed := x.Status == "SyncFailed" || x.HookPhase == model.PhaseFailed || x.HookPhase == model.PhaseError
		if !failed {
			continue
		}
		ref := refOfResult(x)
		b := Blocker{Role: RoleFailed, Ref: ref, ID: ref.ID(), SyncPhase: x.SyncPhase, HookType: x.HookType,
			HookPhase: x.HookPhase, Result: x.Status, Message: x.Message,
			Wave: waveFor(s.App, ref, x.HookType != ""), Source: ".status.operationState.syncResult.resources"}
		if x.HookType != "" {
			b.Type = "hook"
			b.Why = fmt.Sprintf("%s hook ended with phase %s", x.HookType, orUnknown(x.HookPhase))
		} else if isApplicationRef(ref.Group, ref.Kind) {
			b.Type = "child-application"
			b.Why = "applying this child Application, or waiting for its health, failed"
			b.Health = healthFor(s, ref)
		} else {
			b.Type = "resource"
			b.Why = "applying (or waiting for the health of) this resource failed"
			b.Health = healthFor(s, ref)
		}
		r.Blockers = append(r.Blockers, b)
	}
}

func analyzeFinished(s *model.Snapshot, r *Report) {
	st := s.App.Status.OperationState
	opNotes(s, r)
	switch st.Phase {
	case model.PhaseSucceeded:
		// A3(c): finished Succeeded; any remaining rollout is not a blocker of the operation.
		return
	case model.PhaseFailed, model.PhaseError:
		collectFailures(s, r)
		msg := st.Message
		switch {
		case strings.Contains(msg, "(dry run)"):
			r.Notes = append(r.Notes, Note{Code: "failed-in-dry-run",
				Text:   "failed in the apply dry-run before any task ran; SyncFail hooks do not run in this case",
				Source: ".status.operationState.message"})
		case strings.Contains(msg, "retried "):
			r.Notes = append(r.Notes, Note{Code: "retries-exhausted",
				Text:   "the retry limit was reached; Argo CD will not retry this operation again",
				Source: ".status.operationState.message"})
		case strings.Contains(msg, "Operation terminated"):
			r.Notes = append(r.Notes, Note{Code: "terminated", Text: "the operation was terminated", Source: ".status.operationState.message"})
		}
		if st.Phase == model.PhaseFailed && len(r.Blockers) == 0 {
			r.unknown("failing task", "the operation failed but no resource result carries a failure; see the operation message")
		}
	}
}

func analyzeQueued(s *model.Snapshot, r *Report) {
	r.Notes = append(r.Notes, Note{Code: "queued",
		Text:   "a sync was requested (.operation is set) and the application controller has not started it yet",
		Source: ".operation"})
}

// ---- conditions ----

func analyzeConditions(s *model.Snapshot, r *Report) {
	for _, c := range s.App.Status.Conditions {
		r.Conditions = append(r.Conditions, CondInfo{Type: c.Type, Message: c.Message, Source: ".status.conditions"})
	}
}

func hasErrorCondition(a model.Application) bool {
	for _, c := range a.Status.Conditions {
		if strings.HasSuffix(c.Type, "Error") {
			return true
		}
	}
	return false
}

// ---- out of sync (A7) ----

func analyzeOutOfSync(s *model.Snapshot, r *Report) {
	for _, rs := range s.App.Status.Resources {
		if rs.Hook || (rs.Status != "OutOfSync" && !rs.RequiresPruning) {
			continue
		}
		ref := refOfStatus(rs)
		o := OutOfSyncRes{Ref: ref, Status: orUnknown(rs.Status), RequiresPruning: rs.RequiresPruning,
			Source: ".status.resources[]"}
		h := healthFor(s, ref)
		switch {
		case rs.RequiresPruning:
			o.Missing = "no (exists in the cluster, not in the desired state)"
		case h.Status == "Missing":
			o.Missing = "yes (resource health is Missing)"
		case h.Status == Unknown:
			o.Missing = "unknown (OutOfSync covers missing and modified; the Application does not say which: " + h.Message + ")"
		default:
			o.Missing = "no (live object exists, " + h.Status + ")"
		}
		r.OutOfSync = append(r.OutOfSync, o)
	}
	if len(r.OutOfSync) > 0 {
		r.Notes = append(r.Notes, Note{Code: "no-field-diff",
			Text:   "field-level differences are not shown; run `argocd app diff " + s.App.Metadata.Name + "`",
			Source: "-"})
	}
}

// ---- sync windows ----

// RulesFor picks the sync window rules for a server version string such as "v3.5.3+abc".
func RulesFor(version string) (WindowRules, bool) {
	v := strings.TrimPrefix(version, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return WindowRules{SyncOverrun: true}, false
	}
	maj, e1 := strconv.Atoi(parts[0])
	min, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil {
		return WindowRules{SyncOverrun: true}, false
	}
	return WindowRules{SyncOverrun: maj > 3 || (maj == 3 && min >= 5)}, true
}

func analyzeWindows(s *model.Snapshot, r *Report) {
	if s.Project == nil {
		reason := "AppProject not read"
		if why, ok := s.Unavailable["project"]; ok {
			reason = why
		}
		r.unknown("sync windows", "AppProject "+s.App.Spec.Project+" unavailable: "+reason)
		return
	}
	if len(s.Project.Spec.SyncWindows) == 0 {
		return
	}
	rules, known := RulesFor(s.ArgoVersion)
	ruleText := "Argo CD 3.5 rules (syncOverrun supported)"
	if known && !rules.SyncOverrun {
		ruleText = "Argo CD 3.4 rules (no syncOverrun)"
	}
	if !known {
		ruleText = "Argo CD 3.5 rules assumed (server version unknown)"
		r.unknown("Argo CD version", "server version not available; sync windows evaluated with the latest supported rules")
	}
	manual := false
	var opStart *time.Time
	if st := s.App.Status.OperationState; st != nil && r.Operation.Kind != OpNone {
		manual = !st.Operation.InitiatedBy.Automated
		if !st.StartedAt.IsZero() {
			t := st.StartedAt
			opStart = &t
		}
	}
	ev := EvalWindows(s.Project, s.App, s.Now, manual, opStart, rules)
	wi := &WindowInfo{Evaluated: true, CanSync: ev.CanSync, Reason: ev.Reason, Rules: ruleText, Manual: manual,
		Source: "AppProject " + s.Project.Metadata.Name + " .spec.syncWindows evaluated at " + s.Now.UTC().Format(time.RFC3339)}
	for _, w := range ev.Matched {
		wi.Matching = append(wi.Matching, fmt.Sprintf("%s schedule=%q duration=%s tz=%s manualSync=%t", w.Kind, w.Schedule, w.Duration, orUTC(w.TimeZone), w.ManualSync))
	}
	r.SyncWindows = wi
}

func orUTC(s string) string {
	if s == "" {
		return "UTC"
	}
	return s
}

// ---- events ----

func analyzeEvents(s *model.Snapshot, r *Report) {
	n := 0
	for i := len(s.Events) - 1; i >= 0 && n < 5; i-- {
		e := s.Events[i]
		if e.Type != "Warning" {
			continue
		}
		r.Events = append(r.Events, EventInfo{Reason: e.Reason, Message: e.Message, Count: e.Count})
		n++
	}
}

// currentTasks picks the results the operation is waiting on now. The
// controller names the first waiting task in the operation message and counts
// the rest; a result can stay marked Running after its step is over (observed:
// the Namespace created by CreateNamespace=true), so results that are not in
// the named task's syncPhase and wave are reported as stale, not as blockers.
func currentTasks(s *model.Snapshot, r *Report, results []model.ResourceResult, parsed bool, wref Ref, more int) []model.ResourceResult {
	var running []model.ResourceResult
	for _, x := range results {
		if x.HookPhase == model.PhaseRunning {
			running = append(running, x)
		}
	}
	if !parsed {
		return running
	}
	var named *model.ResourceResult
	for i := range running {
		if running[i].Group == wref.Group && running[i].Kind == wref.Kind && running[i].Name == wref.Name {
			named = &running[i]
			break
		}
	}
	if named == nil {
		return running
	}
	nref := refOfResult(*named)
	nwave := waveFor(s.App, nref, named.HookType != "")
	var keep, stale []model.ResourceResult
	for _, x := range running {
		w := waveFor(s.App, refOfResult(x), x.HookType != "")
		sameWave := !isNumber(w) || !isNumber(nwave) || w == nwave
		if x.SyncPhase == named.SyncPhase && sameWave {
			keep = append(keep, x)
		} else {
			stale = append(stale, x)
		}
	}
	if len(stale) > 0 {
		var names []string
		for _, x := range stale {
			names = append(names, fmt.Sprintf("%s/%s (syncPhase %s)", x.Kind, x.Name, orUnknown(x.SyncPhase)))
		}
		r.Notes = append(r.Notes, Note{Code: "stale-running-result",
			Text:   "results still marked Running that are not in the step the controller reports waiting on: " + strings.Join(names, ", "),
			Source: ".status.operationState.syncResult.resources vs .status.operationState.message"})
	}
	if want := 1 + more; len(keep) != want {
		r.Notes = append(r.Notes, Note{Code: "task-count-mismatch",
			Text:   fmt.Sprintf("the controller message counts %d waiting task(s); %d matching result(s) are recorded", want, len(keep)),
			Source: ".status.operationState.message"})
	}
	return keep
}

func isNumber(s string) bool {
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}
