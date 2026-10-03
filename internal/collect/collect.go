// Package collect reads an Application and its context from Argo CD and builds
// a model.Snapshot. Whatever cannot be read is recorded as unavailable with the
// reason; nothing is guessed.
package collect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

// ErrUnavailable marks data that cannot be read in this mode or with these
// permissions. Its message is shown to the user as the reason.
type ErrUnavailable struct{ Reason string }

func (e *ErrUnavailable) Error() string { return e.Reason }

// Source is a read-only view of Argo CD data.
type Source interface {
	Mode() string
	Version(ctx context.Context) (string, error)
	Application(ctx context.Context, ns, name string) (*model.Application, error)
	Project(ctx context.Context, name string) (*model.AppProject, error)
	Tree(ctx context.Context, ns, name string) (*model.ResourceTree, error)
	Events(ctx context.Context, ns, name string) ([]model.Event, error)
}

const maxDepth = 5

// Collect builds the snapshot of one application and, recursively, of the child
// Applications listed in its resources.
func Collect(ctx context.Context, src Source, ns, name string, now time.Time) (*model.Snapshot, error) {
	visited := map[string]bool{}
	ver, verr := src.Version(ctx)
	snap, err := collectOne(ctx, src, ns, name, now, visited, 0, ver)
	if err != nil {
		return nil, err
	}
	if verr != nil {
		snap.Unavailable["version"] = verr.Error()
	}
	return snap, nil
}

func collectOne(ctx context.Context, src Source, ns, name string, now time.Time, visited map[string]bool, depth int, ver string) (*model.Snapshot, error) {
	key := ns + "/" + name
	visited[key] = true
	app, err := src.Application(ctx, ns, name)
	if err != nil {
		return nil, fmt.Errorf("application %s: %w", key, err)
	}
	snap := &model.Snapshot{Mode: src.Mode(), Now: now, ArgoVersion: ver, App: *app,
		Children: map[string]*model.Snapshot{}, Unavailable: map[string]string{}}

	if proj, err := src.Project(ctx, app.Spec.Project); err != nil {
		snap.Unavailable["project"] = reason(err)
	} else {
		snap.Project = proj
	}
	if tree, err := src.Tree(ctx, ns, name); err != nil {
		snap.Unavailable["tree"] = reason(err)
	} else {
		snap.Tree = tree
	}
	if depth == 0 {
		if ev, err := src.Events(ctx, ns, name); err != nil {
			snap.Unavailable["events"] = reason(err)
		} else {
			snap.Events = ev
		}
	}
	for _, rs := range app.Status.Resources {
		if rs.Group != "argoproj.io" || rs.Kind != "Application" || rs.Hook {
			continue
		}
		cns := rs.Namespace
		if cns == "" {
			cns = ns
		}
		ckey := cns + "/" + rs.Name
		if visited[ckey] || depth+1 >= maxDepth {
			continue
		}
		child, err := collectOne(ctx, src, cns, rs.Name, now, visited, depth+1, ver)
		if err != nil {
			snap.Unavailable["child:"+ckey] = reason(errors.Unwrap(err))
			if errors.Unwrap(err) == nil {
				snap.Unavailable["child:"+ckey] = err.Error()
			}
			continue
		}
		snap.Children[ckey] = child
	}
	return snap, nil
}

func reason(err error) string {
	var u *ErrUnavailable
	if errors.As(err, &u) {
		return u.Reason
	}
	return err.Error()
}
