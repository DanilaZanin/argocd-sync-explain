package collect

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func fakeKube(objs ...runtime.Object) *KubeSource {
	scheme := runtime.NewScheme()
	lists := map[schema.GroupVersionResource]string{
		gvrApp: "ApplicationList", gvrProject: "AppProjectList", gvrDeploy: "DeploymentList", gvrEvent: "EventList",
	}
	return NewKubeSource(fake.NewSimpleDynamicClientWithCustomListKinds(scheme, lists, objs...), "argocd")
}

func deploy(containers ...map[string]any) *unstructured.Unstructured {
	var cs []any
	for _, c := range containers {
		cs = append(cs, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "argocd-server", "namespace": "argocd", "labels": map[string]any{"app.kubernetes.io/name": "argocd-server"}},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"containers": cs}}},
	}}
}

func TestKubeVersionComesFromTheArgoCDServerContainer(t *testing.T) {
	k := fakeKube(deploy(
		map[string]any{"name": "log-shipper", "image": "registry/shipper:v1.2.3"},
		map[string]any{"name": "argocd-server", "image": "quay.io/argoproj/argocd:v3.4.9"},
	))
	v, err := k.Version(context.Background())
	if err != nil || v != "v3.4.9" {
		t.Fatalf("%q %v", v, err)
	}
	// Only a sidecar has a tag: the version stays unknown.
	k = fakeKube(deploy(map[string]any{"name": "log-shipper", "image": "registry/shipper:v1.2.3"},
		map[string]any{"name": "argocd-server", "image": "quay.io/argoproj/argocd@sha256:abc"}))
	if v, err := k.Version(context.Background()); err == nil {
		t.Fatalf("a sidecar tag must not become the Argo CD version: %q", v)
	}
}

func TestKubeEventsAreRead(t *testing.T) {
	ev := func(name, ts string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Event",
			"metadata":       map[string]any{"name": name, "namespace": "argocd"},
			"involvedObject": map[string]any{"kind": "Application", "name": "web"},
			"type":           "Warning", "reason": name, "message": "m", "lastTimestamp": ts, "count": int64(1),
		}}
	}
	k := fakeKube(ev("second", "2026-10-03T11:00:02Z"), ev("first", "2026-10-03T11:00:01Z"))
	out, err := k.Events(context.Background(), "argocd", "web")
	if err != nil || len(out) != 2 || out[0].Reason != "first" || out[1].Reason != "second" {
		t.Fatalf("events must be readable through the kubeconfig and ordered: %+v %v", out, err)
	}
}
