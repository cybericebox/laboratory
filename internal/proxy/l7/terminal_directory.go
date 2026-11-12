package l7

import (
	"context"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/names"
)

// PodServiceDirectory finds the device access service on a node from the proxy's cache of its pods: the one pod of the service
// DaemonSet that runs on that node, is Ready and has an address. The cache holds only those pods (one namespace, one label).
func PodServiceDirectory(r client.Reader, namespace string, port int) ServiceDirectory {
	return func(node string) (string, bool) {
		if node == "" {
			return "", false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app": names.DeviceAccessApp}); err != nil {
			return "", false
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Spec.NodeName != node || !p.DeletionTimestamp.IsZero() || p.Status.PodIP == "" || p.Status.Phase != corev1.PodRunning || !podReady(p) {
				continue
			}
			return "http://" + net.JoinHostPort(p.Status.PodIP, strconv.Itoa(port)), true
		}
		return "", false
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
