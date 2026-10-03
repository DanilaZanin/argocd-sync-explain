package explain

import (
	"fmt"
	"strings"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

const resourcesFinalizer = "resources-finalizer.argocd.argoproj.io"

// analyzeDeleting explains an Application that is being deleted but still exists. The Application
// object stays until its finalizers are removed; with the resources finalizer Argo CD deletes the
// managed resources first and waits until none remain, so one resource that holds a finalizer of
// its own keeps the Application around.
func analyzeDeleting(s *model.Snapshot, r *Report) {
	a := s.App
	if a.Metadata.DeletionTimestamp == nil {
		return
	}
	d := &DeletingInfo{Since: fmtTime(a.Metadata.DeletionTimestamp), Finalizers: a.Metadata.Finalizers, Remaining: []RemainingRes{},
		Basis: ".metadata.deletionTimestamp, .metadata.finalizers, live objects of managed resources"}
	if d.Finalizers == nil {
		d.Finalizers = []string{}
	}
	cascade := false
	for _, f := range a.Metadata.Finalizers {
		if strings.HasPrefix(f, resourcesFinalizer) {
			cascade = true
			mode := "default propagation"
			if i := strings.Index(f, "/"); i >= 0 {
				mode = f[i+1:] + " propagation"
			}
			d.Cascade = "cascade (" + mode + "): Argo CD deletes the managed resources and removes the finalizer only when none remain"
		}
	}
	if !cascade {
		d.Cascade = "non-cascade: no resources finalizer, the managed resources are left in place and are not what keeps the Application"
	}
	var others []string
	for _, f := range a.Metadata.Finalizers {
		if !strings.HasPrefix(f, resourcesFinalizer) {
			others = append(others, f)
		}
	}
	if len(others) > 0 {
		r.Notes = append(r.Notes, Note{Code: "other-finalizers",
			Text:   "other finalizers on the Application: " + strings.Join(others, ", ") + " (pre/post-delete hooks or a third party must finish before the object goes away)",
			Source: ".metadata.finalizers"})
	}

	if cascade {
		_, liveWhy := s.Unavailable["live"]
		for _, rs := range a.Status.Resources {
			if rs.Hook {
				continue
			}
			ref := refOfStatus(rs)
			key := ref.ID()
			if lo, ok := s.Live[key]; ok {
				if !lo.Exists {
					continue
				}
				rr := RemainingRes{Ref: ref, Source: "live object"}
				if lo.DeletionTimestamp != nil {
					rr.State = "being deleted since " + fmtTime(lo.DeletionTimestamp)
					rr.Finalizers = lo.Finalizers
					if len(lo.Finalizers) > 0 {
						rr.State += ", held by finalizers " + strings.Join(lo.Finalizers, ", ")
						r.Blockers = append(r.Blockers, Blocker{Role: RoleWaitingOn, Type: "resource", Ref: ref, ID: key, Wave: "-",
							Why:    "deletion of the Application waits for this resource, which has a deletionTimestamp and is held by finalizers: " + strings.Join(lo.Finalizers, ", "),
							Source: "live object .metadata.finalizers"})
					}
				} else {
					rr.State = "exists, not yet marked for deletion"
				}
				d.Remaining = append(d.Remaining, rr)
				continue
			}
			if n := findNode(s.Tree, ref); n != nil && (n.Health == nil || n.Health.Status != "Missing") {
				d.Remaining = append(d.Remaining, RemainingRes{Ref: ref, State: "exists (live details not read)", Source: "resource tree node"})
			} else if s.Tree == nil || liveWhy {
				d.Remaining = append(d.Remaining, RemainingRes{Ref: ref, State: Unknown + " (needs the Argo CD API)", Source: "-"})
			}
		}
		if s.Tree == nil {
			r.unknown("remaining resources", "which managed resources still exist is not visible without the Argo CD API; the list shows all managed resources as unknown")
		}
		if len(d.Remaining) == 0 && s.Tree != nil {
			r.unknown("what holds the deletion", fmt.Sprintf("no managed resource is visible as remaining; finalizers on the Application: %s", strings.Join(a.Metadata.Finalizers, ", ")))
		}
	}
	r.Deleting = d
}
