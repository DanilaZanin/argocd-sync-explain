package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/DanilaZanin/argocd-sync-explain/internal/model"
)

var (
	gvrApp     = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	gvrProject = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "appprojects"}
	gvrDeploy  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

// KubeSource reads Application and AppProject custom resources through the
// Kubernetes API. It cannot see the resource tree or events through Argo CD.
type KubeSource struct {
	Dyn    dynamic.Interface
	ArgoNS string
}

// NewKubeSource builds a kubeconfig-only source. argoNS is the namespace of the
// Argo CD installation (AppProjects and the server Deployment live there).
func NewKubeSource(dyn dynamic.Interface, argoNS string) *KubeSource {
	return &KubeSource{Dyn: dyn, ArgoNS: argoNS}
}

// Mode implements Source.
func (k *KubeSource) Mode() string { return "kubeconfig" }

func kerr(err error, what string) error {
	switch {
	case apierrors.IsForbidden(err):
		return &ErrUnavailable{Reason: what + ": forbidden by Kubernetes RBAC"}
	case apierrors.IsNotFound(err):
		return &ErrUnavailable{Reason: what + ": not found"}
	}
	return err
}

// Version reads the image tag of the argocd-server Deployment.
func (k *KubeSource) Version(ctx context.Context) (string, error) {
	l, err := k.Dyn.Resource(gvrDeploy).Namespace(k.ArgoNS).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=argocd-server"})
	if err != nil {
		return "", kerr(err, "argocd-server Deployment")
	}
	if len(l.Items) == 0 {
		return "", &ErrUnavailable{Reason: "no argocd-server Deployment in namespace " + k.ArgoNS}
	}
	cs, _ := unstructuredSlice(l.Items[0].Object, "spec", "template", "spec", "containers")
	for _, c := range cs {
		m, _ := c.(map[string]any)
		img, _ := m["image"].(string)
		if i := strings.LastIndex(img, ":"); i > 0 && !strings.Contains(img[i:], "/") && !strings.Contains(img, "@") {
			return img[i+1:], nil
		}
	}
	return "", &ErrUnavailable{Reason: "argocd-server image has no version tag (digest-pinned)"}
}

func unstructuredSlice(obj map[string]any, path ...string) ([]any, bool) {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = m[p]
	}
	s, ok := cur.([]any)
	return s, ok
}

func decode(obj map[string]any, out any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// Application implements Source.
func (k *KubeSource) Application(ctx context.Context, ns, name string) (*model.Application, error) {
	u, err := k.Dyn.Resource(gvrApp).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, kerr(err, fmt.Sprintf("Application %s/%s", ns, name))
	}
	var a model.Application
	if err := decode(u.Object, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Project implements Source.
func (k *KubeSource) Project(ctx context.Context, name string) (*model.AppProject, error) {
	u, err := k.Dyn.Resource(gvrProject).Namespace(k.ArgoNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, kerr(err, "AppProject "+name)
	}
	var p model.AppProject
	if err := decode(u.Object, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Tree is not available without the Argo CD API.
func (k *KubeSource) Tree(context.Context, string, string) (*model.ResourceTree, error) {
	return nil, &ErrUnavailable{Reason: "needs the Argo CD API (the resource tree is served by argocd-server, not stored in the Application)"}
}

// Events is not read in kubeconfig mode.
func (k *KubeSource) Events(context.Context, string, string) ([]model.Event, error) {
	return nil, &ErrUnavailable{Reason: "needs the Argo CD API"}
}
