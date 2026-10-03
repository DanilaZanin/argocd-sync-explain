package explain

import (
	"fmt"
	"time"

	"github.com/gobwas/glob"
	"github.com/robfig/cron/v3"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

// Sync window evaluation. This is a port of the decision logic in
// pkg/apis/application/v1alpha1/types.go (SyncWindows.Matches, Active,
// InactiveAllows, CanSync) and controller/sync.go (syncWindowPreventsSync).
// Argo CD 3.5 added syncOverrun; 3.4 does not have it, so the rules depend on
// the server version (see WindowRules).

// WindowRules selects version-dependent behaviour.
type WindowRules struct {
	// SyncOverrun is true for Argo CD >= 3.5.
	SyncOverrun bool
}

// WindowEval is the outcome for one application at one moment.
type WindowEval struct {
	CanSync        bool
	Manual         bool
	Matched        []model.SyncWindow
	ActiveDeny     []model.SyncWindow
	ActiveAllow    []model.SyncWindow
	InactiveAllow  []model.SyncWindow
	Reason         string
	OverrunApplied bool
	Err            string
}

func globMatch(pattern, val string) bool {
	if pattern == "*" {
		return true
	}
	g, err := glob.Compile(pattern)
	if err != nil {
		return false
	}
	return g.Match(val)
}

// matchWindows mirrors SyncWindows.Matches.
func matchWindows(all []model.SyncWindow, app model.Application) []model.SyncWindow {
	var out []model.SyncWindow
	for _, w := range all {
		matched, isSet := false, false
		if len(w.Applications) > 0 {
			isSet = true
			for _, a := range w.Applications {
				if globMatch(a, app.Metadata.Name) {
					matched = true
					break
				}
			}
		}
		if w.UseAndOperator && !matched && isSet {
			continue
		} else if !w.UseAndOperator && matched {
			out = append(out, w)
			continue
		}
		if len(w.Clusters) > 0 {
			matched = false
			isSet = true
			dst := app.Spec.Destination
			for _, c := range w.Clusters {
				if (dst.Name != "" && globMatch(c, dst.Name)) || (dst.Server != "" && globMatch(c, dst.Server)) {
					matched = true
					break
				}
			}
		}
		if isSet && w.UseAndOperator && !matched {
			continue
		} else if !w.UseAndOperator && matched {
			out = append(out, w)
			continue
		}
		if len(w.Namespaces) > 0 {
			matched = false
			for _, n := range w.Namespaces {
				if globMatch(n, app.Spec.Destination.Namespace) {
					matched = true
					break
				}
			}
		}
		if matched {
			out = append(out, w)
		}
	}
	return out
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// tzOffset mirrors scheduleOffsetByTimeZone: an unknown zone falls back to UTC.
// Argo CD takes the offset at the evaluation instant; so do we.
func tzOffset(w model.SyncWindow, at time.Time) time.Duration {
	loc, err := time.LoadLocation(w.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	_, off := at.In(loc).Zone()
	return time.Duration(off) * time.Second
}

func windowActive(w model.SyncWindow, at time.Time) (bool, error) {
	at = at.UTC()
	sched, err := cronParser.Parse(w.Schedule)
	if err != nil {
		return false, fmt.Errorf("cannot parse schedule '%s': %w", w.Schedule, err)
	}
	dur, err := time.ParseDuration(w.Duration)
	if err != nil {
		return false, fmt.Errorf("cannot parse duration '%s': %w", w.Duration, err)
	}
	off := tzOffset(w, at)
	next := sched.Next(at.Add(off - dur))
	return next.Before(at.Add(off)), nil
}

func activeSet(ws []model.SyncWindow, at time.Time) (act []model.SyncWindow, inactiveAllow []model.SyncWindow, err error) {
	for _, w := range ws {
		a, e := windowActive(w, at)
		if e != nil {
			return nil, nil, e
		}
		if a {
			act = append(act, w)
		} else if w.Kind == "allow" {
			inactiveAllow = append(inactiveAllow, w)
		}
	}
	return act, inactiveAllow, nil
}

func hasDeny(ws []model.SyncWindow) (found, manual bool) {
	for _, w := range ws {
		if w.Kind == "deny" {
			if !found {
				manual = w.ManualSync
			} else if manual {
				manual = w.ManualSync
			}
			found = true
		}
	}
	return
}

func hasKind(ws []model.SyncWindow, kind string) bool {
	for _, w := range ws {
		if w.Kind == kind {
			return true
		}
	}
	return false
}

func allManual(ws []model.SyncWindow) bool {
	if len(ws) == 0 {
		return false
	}
	for _, w := range ws {
		if !w.ManualSync {
			return false
		}
	}
	return true
}

func allOverrun(ws []model.SyncWindow, kind string) bool {
	any := false
	for _, w := range ws {
		if w.Kind == kind {
			any = true
			if !w.SyncOverrun {
				return false
			}
		}
	}
	return any
}

func splitKinds(ws []model.SyncWindow) (deny, allow []model.SyncWindow) {
	for _, w := range ws {
		switch w.Kind {
		case "deny":
			deny = append(deny, w)
		case "allow":
			allow = append(allow, w)
		}
	}
	return
}

// canSyncAt mirrors SyncWindows.canSyncAtTime (no overrun consideration).
func canSyncAt(ws []model.SyncWindow, manual bool, at time.Time) (bool, error) {
	if len(ws) == 0 {
		return true, nil
	}
	act, inactiveAllow, err := activeSet(ws, at)
	if err != nil {
		return false, err
	}
	if deny, man := hasDeny(act); deny {
		return manual && man, nil
	}
	if hasKind(act, "allow") {
		return true, nil
	}
	if len(inactiveAllow) > 0 {
		return manual && allManual(inactiveAllow), nil
	}
	return true, nil
}

// EvalWindows decides whether a sync of app is allowed at now. isManual is
// whether the sync was (or would be) started by a person; opStart is the start
// of a running operation, or nil.
func EvalWindows(proj *model.AppProject, app model.Application, now time.Time, isManual bool, opStart *time.Time, rules WindowRules) WindowEval {
	ev := WindowEval{CanSync: true, Manual: isManual}
	if proj == nil {
		return ev
	}
	matched := matchWindows(proj.Spec.SyncWindows, app)
	ev.Matched = matched
	if len(matched) == 0 {
		ev.Reason = "no sync window matches this application"
		return ev
	}
	act, inactiveAllow, err := activeSet(matched, now)
	if err != nil {
		ev.CanSync = false
		ev.Err = err.Error()
		ev.Reason = "sync windows are invalid: " + err.Error()
		return ev
	}
	ev.ActiveDeny, ev.ActiveAllow = splitKinds(act)
	ev.InactiveAllow = inactiveAllow

	opWasAllowed := func(kindForOverrun []model.SyncWindow, kind string) bool {
		if !rules.SyncOverrun || opStart == nil || opStart.IsZero() || !allOverrun(kindForOverrun, kind) {
			return false
		}
		ok, e := canSyncAt(matched, isManual, *opStart)
		return e == nil && ok
	}

	if deny, man := hasDeny(act); deny {
		if isManual && man {
			ev.Reason = "a deny window is active, but manualSync is enabled on all active deny windows"
			return ev
		}
		if opWasAllowed(act, "deny") {
			ev.OverrunApplied = true
			ev.Reason = "a deny window is active; the running operation started while syncs were allowed and syncOverrun is set"
			return ev
		}
		ev.CanSync = false
		ev.Reason = "a deny window is active"
		return ev
	}
	if hasKind(act, "allow") {
		ev.Reason = "an allow window is active"
		return ev
	}
	if len(inactiveAllow) > 0 {
		if isManual && allManual(inactiveAllow) {
			ev.Reason = "no allow window is active, but manualSync is enabled on all allow windows"
			return ev
		}
		if opWasAllowed(inactiveAllow, "allow") {
			ev.OverrunApplied = true
			ev.Reason = "no allow window is active now; the running operation started inside one and syncOverrun is set"
			return ev
		}
		ev.CanSync = false
		ev.Reason = "allow windows exist for this application but none is active now"
		return ev
	}
	ev.Reason = "matching windows are not active"
	return ev
}
