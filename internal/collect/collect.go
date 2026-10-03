// Package collect reads an Application and its context from Argo CD and builds
// a model.Snapshot. Whatever cannot be read is recorded as unavailable with the
// reason; nothing is guessed.
package collect

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

// ErrUnavailable marks data that cannot be read in this mode or with these
// permissions. Its message is shown to the user as the reason.
type ErrUnavailable struct {
	Reason string
	// Denied is true for an authorization failure (HTTP 403, Kubernetes Forbidden).
	Denied bool
}

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

// LiveSource can read live managed objects (API mode only). It is used to explain deletion.
type LiveSource interface {
	Live(ctx context.Context, ns, name string, ref model.ResourceStatus) (*model.LiveObject, error)
}

// MaxDepth is the number of Application levels that are read (the root is level 1).
const MaxDepth = 5

const maxLive = 50

type walker struct {
	src    Source
	now    time.Time
	ver    string
	done   map[string]*model.Snapshot
	active map[string]bool
}

// Collect builds the snapshot of one application and, recursively, of the child
// Applications listed in its resources.
func Collect(ctx context.Context, src Source, ns, name string, now time.Time) (*model.Snapshot, error) {
	ver, verr := src.Version(ctx)
	w := &walker{src: src, now: now, ver: ver, done: map[string]*model.Snapshot{}, active: map[string]bool{}}
	snap, err := w.one(ctx, ns, name, 0)
	if err != nil {
		return nil, err
	}
	if verr != nil {
		snap.Unavailable["version"] = reason(verr)
	}
	return snap, nil
}

// inCluster reports whether Application objects created by an app with this destination live in the
// cluster Argo CD runs in (the only place this tool reads Applications from).
func inCluster(d model.Destination) bool {
	return d.Server == "https://kubernetes.default.svc" || d.Name == "in-cluster"
}

func (w *walker) one(ctx context.Context, ns, name string, depth int) (*model.Snapshot, error) {
	key := ns + "/" + name
	if s, ok := w.done[key]; ok {
		return s, nil
	}
	w.active[key] = true
	defer delete(w.active, key)
	app, err := w.src.Application(ctx, ns, name)
	if err != nil {
		return nil, fmt.Errorf("application %s: %w", key, err)
	}
	snap := &model.Snapshot{Mode: w.src.Mode(), Now: w.now, ArgoVersion: w.ver, App: *app,
		Children: map[string]*model.Snapshot{}, Unavailable: map[string]string{}}
	w.done[key] = snap

	if proj, err := w.src.Project(ctx, app.Spec.Project); err != nil {
		snap.Unavailable["project"] = reason(err)
	} else {
		snap.Project = proj
	}
	if tree, err := w.src.Tree(ctx, ns, name); err != nil {
		snap.Unavailable["tree"] = reason(err)
	} else {
		snap.Tree = tree
	}
	if depth == 0 {
		if ev, err := w.src.Events(ctx, ns, name); err != nil {
			snap.Unavailable["events"] = reason(err)
		} else {
			snap.Events = ev
		}
	}
	if app.Metadata.DeletionTimestamp != nil {
		w.live(ctx, ns, name, snap)
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
		switch {
		case !inCluster(app.Spec.Destination):
			snap.Unavailable["child:"+ckey] = fmt.Sprintf("the parent's destination (%s%s) is not the cluster Argo CD runs in, so this Application object is not read from here",
				app.Spec.Destination.Server, app.Spec.Destination.Name)
		case w.active[ckey]:
			snap.Unavailable["child:"+ckey] = "cycle: this Application is an ancestor of itself; not followed again"
		case depth+1 >= MaxDepth:
			snap.Unavailable["child:"+ckey] = fmt.Sprintf("depth limit of %d levels reached; this child and its descendants are not read", MaxDepth)
		default:
			child, err := w.one(ctx, cns, rs.Name, depth+1)
			if err != nil {
				var u *ErrUnavailable
				if errors.As(err, &u) {
					snap.Unavailable["child:"+ckey] = u.Reason
				} else {
					snap.Unavailable["child:"+ckey] = err.Error()
				}
				continue
			}
			snap.Children[ckey] = child
		}
	}
	return snap, nil
}

// live reads live details of the managed resources of an Application that is being deleted.
func (w *walker) live(ctx context.Context, ns, name string, snap *model.Snapshot) {
	ls, ok := w.src.(LiveSource)
	if !ok {
		snap.Unavailable["live"] = "needs the Argo CD API (live managed objects are not read through the Application)"
		return
	}
	snap.Live = map[string]*model.LiveObject{}
	n := 0
	for _, rs := range snap.App.Status.Resources {
		if rs.Hook {
			continue
		}
		if snap.Tree != nil {
			found := false
			for _, nd := range snap.Tree.Nodes {
				if nd.Group == rs.Group && nd.Kind == rs.Kind && nd.Namespace == rs.Namespace && nd.Name == rs.Name {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if n++; n > maxLive {
			snap.Unavailable["live"] = fmt.Sprintf("only the first %d managed resources were inspected", maxLive)
			return
		}
		lo, err := ls.Live(ctx, ns, name, rs)
		if err != nil {
			snap.Unavailable["live"] = reason(err)
			return
		}
		snap.Live[fmt.Sprintf("%s/%s/%s/%s", rs.Group, rs.Kind, rs.Namespace, rs.Name)] = lo
	}
}

func reason(err error) string {
	var u *ErrUnavailable
	if errors.As(err, &u) {
		return u.Reason
	}
	return err.Error()
}

var (
	jwtRe    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.?[A-Za-z0-9_-]*`)
	bearerRe = regexp.MustCompile(`(?i)(bearer|token)(["':=\s]+)[A-Za-z0-9._~+/=-]{12,}`)
)

// Redact removes tokens from text that may be shown to the user: the given secrets, JWT-like
// strings and "Bearer ..." values.
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 6 {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = jwtRe.ReplaceAllString(s, "[redacted]")
	return bearerRe.ReplaceAllString(s, "${1}${2}[redacted]")
}

func sortEvents(ev []model.Event) {
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].LastTimestamp.Before(ev[j].LastTimestamp) })
}
