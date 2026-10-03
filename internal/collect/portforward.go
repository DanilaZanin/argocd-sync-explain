package collect

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForward forwards a random local port to port 8080 of a running
// argocd-server pod found through the kubeconfig, like `argocd --port-forward`.
// It returns the local address and a stop function.
func PortForward(ctx context.Context, cfg *rest.Config, argoNS string) (string, func(), error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", nil, err
	}
	pods, err := cs.CoreV1().Pods(argoNS).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=argocd-server"})
	if err != nil {
		return "", nil, fmt.Errorf("list argocd-server pods in %s: %w", argoNS, err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			pod = p
			break
		}
	}
	if pod == nil {
		return "", nil, fmt.Errorf("no running argocd-server pod in namespace %s", argoNS)
	}
	rt, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return "", nil, err
	}
	u := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(argoNS).Name(pod.Name).SubResource("portforward").URL()
	spdyDialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)
	dialer := spdyDialer
	if wd, werr := portforward.NewSPDYOverWebsocketDialer(&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}, cfg); werr == nil {
		dialer = fallback(wd, spdyDialer)
	}
	stop := make(chan struct{})
	ready := make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:8080"}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return "", nil, err
	}
	errc := make(chan error, 1)
	go func() { errc <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-errc:
		return "", nil, fmt.Errorf("port-forward to %s/%s: %w", argoNS, pod.Name, err)
	case <-ctx.Done():
		close(stop)
		return "", nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stop)
		return "", nil, fmt.Errorf("port-forward: no local port: %v", err)
	}
	return fmt.Sprintf("127.0.0.1:%d", ports[0].Local), func() { close(stop) }, nil
}
