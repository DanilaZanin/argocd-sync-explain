// Package render prints a Report as text or JSON.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/DanilaZanin/argocd-sync-explain/internal/explain"
)

// JSON writes the report as indented JSON.
func JSON(w io.Writer, r *explain.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Text writes the human-readable report.
func Text(w io.Writer, r *explain.Report) {
	p := &printer{w: w}
	p.report(r, "", true)
}

type printer struct{ w io.Writer }

func (p *printer) f(indent, format string, a ...any) {
	fmt.Fprintf(p.w, indent+format+"\n", a...)
}

func (p *printer) report(r *explain.Report, in string, top bool) {
	a := r.Application
	p.f(in, "%s/%s  sync=%s health=%s  project=%s", a.Namespace, a.Name, a.Sync, a.Health, a.Project)
	if top {
		ver := r.ArgoVersion
		if ver == "" {
			ver = "version unknown"
		}
		p.f(in, "mode: %s (Argo CD %s)", r.Mode, ver)
	}
	if a.ManagedBy != "" {
		p.f(in, "managed by: %s", a.ManagedBy)
	}
	p.f(in, "")
	p.f(in, "VERDICT: %s", strings.ToUpper(r.Verdict))
	p.f(in, "  %s", r.Summary)

	op := r.Operation
	p.f(in, "")
	p.f(in, "Operation: %s%s", op.Kind, opDetail(op))
	if op.Message != "" {
		p.f(in, "  message: %s", oneLine(op.Message))
	}
	if op.Retry != nil && (op.Kind == explain.OpBackoff || op.Kind == explain.OpFinished) {
		lim := fmt.Sprintf("%d", op.Retry.Limit)
		if op.Retry.Unlimited {
			lim = "unlimited"
		}
		p.f(in, "  retry: limit=%s, %s (%s)", lim, op.Retry.Backoff, op.Retry.Source)
	}
	if op.NextRetryNotBefore != "" {
		p.f(in, "  next attempt: not before %s", op.NextRetryNotBefore)
	}
	if r.Position != nil {
		p.f(in, "  position (reconstructed): phase=%s wave=%s", r.Position.Phase, r.Position.Wave)
		p.f(in, "    basis: %s", r.Position.Basis)
	}

	if len(r.Blockers) > 0 {
		p.f(in, "")
		p.f(in, "Blockers:")
		for _, b := range r.Blockers {
			p.f(in, "  [%s] %s %s", b.Role, b.Type, display(b.Ref))
			p.f(in, "      %s", b.Why)
			if b.SyncPhase != "" || b.HookType != "" {
				p.f(in, "      syncPhase=%s hookType=%s hookPhase=%s wave=%s", dash(b.SyncPhase), dash(b.HookType), dash(b.HookPhase), b.Wave)
			} else if b.Wave != "-" {
				p.f(in, "      wave=%s", b.Wave)
			}
			if b.Health != nil {
				p.f(in, "      health: %s%s  [%s]", b.Health.Status, msg(b.Health.Message), b.Health.Source)
			}
			if b.Message != "" {
				p.f(in, "      message: %s", oneLine(b.Message))
			}
			p.f(in, "      source: %s", b.Source)
		}
	}
	if r.Pending != nil {
		p.f(in, "")
		p.f(in, "Pending (no result yet, %d): %s  waves=%v", r.Pending.Count, strings.Join(r.Pending.Resources, ", "), r.Pending.Waves)
		p.f(in, "    %s", r.Pending.Caveat)
	}
	if len(r.Observation) > 0 {
		p.f(in, "")
		p.f(in, "Observations (context, not blockers):")
		for _, o := range r.Observation {
			h := ""
			if o.Health != nil {
				h = fmt.Sprintf(" health=%s%s", o.Health.Status, msg(o.Health.Message))
			}
			info := ""
			if o.Info != "" {
				info = " " + o.Info
			}
			p.f(in, "  under %s: %s%s%s", o.Under, display(o.Ref), h, info)
		}
	}
	if d := r.Deleting; d != nil {
		p.f(in, "")
		p.f(in, "Deletion (since %s): %s", d.Since, d.Cascade)
		p.f(in, "  finalizers on the Application: %s", strings.Join(d.Finalizers, ", "))
		for _, x := range d.Remaining {
			p.f(in, "  remaining: %s %s  [%s]", display(x.Ref), x.State, x.Source)
		}
	}
	if len(r.OutOfSync) > 0 {
		p.f(in, "")
		p.f(in, "OutOfSync resources:")
		for _, o := range r.OutOfSync {
			pr := ""
			if o.RequiresPruning {
				pr = " requires-pruning"
			}
			p.f(in, "  %s%s  missing: %s", display(o.Ref), pr, o.Missing)
		}
	}
	if r.AutoSync != nil {
		p.f(in, "")
		p.f(in, "Auto-sync (enabled=%t):", r.AutoSync.Enabled)
		for _, f := range r.AutoSync.Verified {
			p.f(in, "  verified: %s  [%s]", f.Text, f.Source)
		}
		for _, f := range r.AutoSync.Possible {
			p.f(in, "  possible (not provable from the snapshot): %s  [%s]", f.Text, f.Source)
		}
	}
	if w := r.SyncWindows; w != nil {
		p.f(in, "")
		p.f(in, "Sync windows (%s): canSync=%t, %s", w.Rules, w.CanSync, w.Reason)
		for _, m := range w.Matching {
			p.f(in, "  matches: %s", m)
		}
	}
	if len(r.Conditions) > 0 {
		p.f(in, "")
		p.f(in, "Conditions:")
		for _, c := range r.Conditions {
			p.f(in, "  %s: %s", c.Type, oneLine(c.Message))
		}
	}
	if len(r.Events) > 0 {
		p.f(in, "")
		p.f(in, "Recent warning events:")
		for _, e := range r.Events {
			p.f(in, "  %s: %s", e.Reason, oneLine(e.Message))
		}
	}
	if len(r.Notes) > 0 {
		p.f(in, "")
		p.f(in, "Notes:")
		for _, n := range r.Notes {
			p.f(in, "  %s  [%s]", n.Text, n.Source)
		}
	}
	for _, c := range r.Children {
		p.f(in, "")
		healthy := c.Report != nil && c.Report.Verdict == explain.VerdictHealthy && len(c.Report.Children) == 0 && len(c.Report.Unknowns) == 0
		p.f(in, "Child Application %s: %s", c.Ref.Name, c.Relation)
		p.f(in, "  %s", c.Evidence)
		if c.Report != nil && !healthy {
			p.f(in, "  explained independently:")
			p.report(c.Report, in+"    | ", false)
		} else if healthy {
			p.f(in, "  child is healthy and synced")
		}
	}
	if len(r.Unknowns) > 0 {
		p.f(in, "")
		p.f(in, "Unknown (not guessed):")
		for _, u := range r.Unknowns {
			p.f(in, "  %s: unknown (%s)", u.What, u.Reason)
		}
	}
}

func opDetail(op explain.OperationInfo) string {
	var parts []string
	if op.Phase != "" {
		parts = append(parts, "phase="+op.Phase)
	}
	if op.Initiator != "" {
		parts = append(parts, op.Initiator)
	}
	if op.StartedAt != "" {
		parts = append(parts, "started "+op.StartedAt)
	}
	if op.FinishedAt != "" {
		parts = append(parts, "finished "+op.FinishedAt)
	}
	if op.RetryCount > 0 {
		parts = append(parts, fmt.Sprintf("retryCount=%d", op.RetryCount))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func display(r explain.Ref) string {
	s := r.Kind + "/" + r.Name
	if r.Namespace != "" {
		s += " (ns " + r.Namespace + ")"
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func msg(s string) string {
	if s == "" {
		return ""
	}
	return ": " + oneLine(s)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}
