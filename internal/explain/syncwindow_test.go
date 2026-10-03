package explain

import (
	"testing"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

func win(kind, sched, dur string, mut func(*model.SyncWindow)) model.SyncWindow {
	w := model.SyncWindow{Kind: kind, Schedule: sched, Duration: dur, Applications: []string{"*"}}
	if mut != nil {
		mut(&w)
	}
	return w
}

func proj(ws ...model.SyncWindow) *model.AppProject {
	p := &model.AppProject{}
	p.Metadata.Name = "p"
	p.Spec.SyncWindows = ws
	return p
}

func TestEvalWindows(t *testing.T) {
	app := model.Application{}
	app.Metadata.Name = "web"
	app.Spec.Destination = model.Destination{Server: "https://kubernetes.default.svc", Namespace: "prod"}
	// 2026-10-03 12:30 UTC. (Argo CD treats the exact start instant of a window as not yet active.)
	noon := time.Date(2026, 10, 3, 12, 30, 0, 0, time.UTC)
	rules35 := WindowRules{SyncOverrun: true}
	rules34 := WindowRules{}
	started := noon.Add(-45 * time.Minute)

	cases := []struct {
		name     string
		p        *model.AppProject
		manual   bool
		opStart  *time.Time
		rules    WindowRules
		want     bool
		contains string
	}{
		{"no project", nil, false, nil, rules35, true, ""},
		{"no windows", proj(), false, nil, rules35, true, ""},
		{"window does not match the app", proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) { w.Applications = []string{"other"} })), false, nil, rules35, true, "no sync window matches"},
		{"always-on deny", proj(win("deny", "* * * * *", "1h", nil)), false, nil, rules35, false, "deny window is active"},
		{"deny manualSync allows manual", proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) { w.ManualSync = true })), true, nil, rules35, true, "manualSync"},
		{"deny manualSync blocks automated", proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) { w.ManualSync = true })), false, nil, rules35, false, "deny window is active"},
		{"deny not active (hour 3-4)", proj(win("deny", "0 3 * * *", "1h", nil)), false, nil, rules35, true, "not active"},
		{"allow window inactive blocks", proj(win("allow", "0 3 * * *", "1h", nil)), false, nil, rules35, false, "none is active"},
		{"allow window active allows", proj(win("allow", "0 12 * * *", "1h", nil)), false, nil, rules35, true, "allow window is active"},
		{"inactive allow with manualSync lets manual through", proj(win("allow", "0 3 * * *", "1h", func(w *model.SyncWindow) { w.ManualSync = true })), true, nil, rules35, true, "manualSync"},
		{"deny wins over active allow", proj(win("allow", "0 12 * * *", "2h", nil), win("deny", "30 11 * * *", "2h", nil)), false, nil, rules35, false, "deny window is active"},
		{"timezone: 15:00 in UTC+3 is 12:00 UTC", proj(win("deny", "0 15 * * *", "1h", func(w *model.SyncWindow) { w.TimeZone = "Europe/Moscow" })), false, nil, rules35, false, "deny"},
		{"timezone: same schedule in UTC is not active", proj(win("deny", "0 15 * * *", "1h", nil)), false, nil, rules35, true, ""},
		{"andOperator with non-matching namespace", proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) {
			w.UseAndOperator = true
			w.Applications = []string{"web"}
			w.Namespaces = []string{"dev"}
		})), false, nil, rules35, true, ""},
		{"andOperator with matching app and namespace", proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) {
			w.UseAndOperator = true
			w.Applications = []string{"web"}
			w.Namespaces = []string{"prod"}
		})), false, nil, rules35, false, "deny"},
		{"OR operator: namespace matches, app does not", proj(win("deny", "* * * * *", "1h", func(w *model.SyncWindow) {
			w.Applications = []string{"other"}
			w.Namespaces = []string{"prod"}
		})), false, nil, rules35, false, "deny"},
		{"deny syncOverrun lets a running op continue on 3.5", proj(win("deny", "0 12 * * *", "1h", func(w *model.SyncWindow) { w.SyncOverrun = true })), false, &started, rules35, true, "syncOverrun"},
		{"same on 3.4 has no syncOverrun", proj(win("deny", "0 12 * * *", "1h", func(w *model.SyncWindow) { w.SyncOverrun = true })), false, &started, rules34, false, "deny window is active"},
		{"overrun does not apply when the op started inside the deny window", proj(win("deny", "0 11 * * *", "2h", func(w *model.SyncWindow) { w.SyncOverrun = true })), false, &started, rules35, false, "deny window is active"},
		{"invalid schedule blocks with an error", proj(win("deny", "not a cron", "1h", nil)), false, nil, rules35, false, "invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := EvalWindows(c.p, app, noon, c.manual, c.opStart, c.rules)
			if ev.CanSync != c.want {
				t.Fatalf("CanSync=%v want %v (%s)", ev.CanSync, c.want, ev.Reason)
			}
			if c.contains != "" && !contains(ev.Reason, c.contains) {
				t.Fatalf("reason %q does not contain %q", ev.Reason, c.contains)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRulesFor(t *testing.T) {
	cases := map[string]struct{ overrun, known bool }{
		"v3.5.3":        {true, true},
		"v3.5.0+abc":    {true, true},
		"v3.4.9":        {false, true},
		"3.4.9":         {false, true},
		"v3.6.0-rc1":    {true, true},
		"v4.0.0":        {true, true},
		"v2.14.1":       {false, true},
		"":              {true, false},
		"not-a-version": {true, false},
	}
	for in, want := range cases {
		r, known := RulesFor(in)
		if r.SyncOverrun != want.overrun || known != want.known {
			t.Errorf("%q: got overrun=%v known=%v want %+v", in, r.SyncOverrun, known, want)
		}
	}
}

func TestNextRetryAt(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	f := int64(3)
	cases := []struct {
		name  string
		rs    model.RetryStrategy
		count int64
		want  time.Duration
	}{
		{"defaults first retry", model.RetryStrategy{Limit: 5}, 0, 5 * time.Second},
		{"defaults second", model.RetryStrategy{Limit: 5}, 1, 10 * time.Second},
		{"defaults capped at 3m", model.RetryStrategy{Limit: 5}, 10, 180 * time.Second},
		{"custom with max", model.RetryStrategy{Backoff: &model.Backoff{Duration: "1h", MaxDuration: "2h"}}, 1, 2 * time.Hour},
		{"bare seconds and factor", model.RetryStrategy{Backoff: &model.Backoff{Duration: "10", Factor: &f, MaxDuration: "1h"}}, 2, 90 * time.Second},
	}
	for _, c := range cases {
		got, err := nextRetryAt(c.rs, base, c.count)
		if err != nil || got.Sub(base) != c.want {
			t.Errorf("%s: got %v (err %v), want %v", c.name, got.Sub(base), err, c.want)
		}
	}
	if _, err := nextRetryAt(model.RetryStrategy{Backoff: &model.Backoff{Duration: "soon"}}, base, 0); err == nil {
		t.Error("unparseable duration must be an error")
	}
}
