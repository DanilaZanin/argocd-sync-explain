package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DanilaZanin/argocd-sync-explain/internal/explain"
)

func TestHealthyChildWithUnknownsIsNotCollapsed(t *testing.T) {
	child := &explain.Report{Verdict: explain.VerdictHealthy, Summary: "Synced and Healthy.",
		Unknowns: []explain.UnknownItem{{What: "sync windows", Reason: "AppProject default unavailable: permission denied"}}}
	child.Application.Name = "child"
	parent := &explain.Report{Verdict: explain.VerdictHealthy, Summary: "ok",
		Children: []explain.ChildInfo{{Ref: explain.Ref{Kind: "Application", Name: "child"}, Relation: explain.RelCanWait, Evidence: "e", Report: child}}}
	var b bytes.Buffer
	Text(&b, parent)
	if !strings.Contains(b.String(), "permission denied") || strings.Contains(b.String(), "child is healthy and synced") {
		t.Fatalf("the RBAC unknown of a healthy child must be printed:\n%s", b.String())
	}
	child.Unknowns = nil
	b.Reset()
	Text(&b, parent)
	if !strings.Contains(b.String(), "child is healthy and synced") {
		t.Fatalf("a fully known healthy child is collapsed:\n%s", b.String())
	}
}

func TestChildHeaderHasTheNamespace(t *testing.T) {
	parent := &explain.Report{Verdict: explain.VerdictHealthy, Summary: "ok",
		Children: []explain.ChildInfo{{Ref: explain.Ref{Kind: "Application", Namespace: "team-a", Name: "dup"}, Relation: explain.RelUnknown, Evidence: "e"}}}
	var b bytes.Buffer
	Text(&b, parent)
	if !strings.Contains(b.String(), "Child Application team-a/dup") {
		t.Fatal(b.String())
	}
}
