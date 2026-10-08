package laboratory

import (
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/tenant"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func exactStoppedRelease(l *lab.Lab) bool {
	intent, observed, resources := l.Spec.Lifecycle, l.Status.Lifecycle, l.Status.Resources
	return intent != nil && observed != nil && resources != nil && observed.ObservedState == "Stopped" && observed.LabUID == string(l.UID) && observed.ObservedGeneration == l.Generation && observed.OperationID == intent.OperationID && observed.Revision == intent.Revision && resources.RuntimeState == "Released" && resources.OperationID == intent.OperationID && resources.Revision == intent.Revision
}

// Stop removes objects from dispatch, never their held reservations. The real
// Deployment template preserves its last actual requests; bare Pods fall back
// to retained Device configuration until Task5 supplies native allocation data.
// Unknown placement/requests holds admission in this scheduler's affected pool.
func (s *Scheduler) stoppedReservations(snap *clusterView) ([]corev1.Pod, map[string]tenant.Totals, bool) {
	var synthetic []corev1.Pod
	held := map[string]tenant.Totals{}
	unknown := false
	for _, l := range snap.labs {
		if !l.Spec.Lifecycle.IsStopped() || exactStoppedRelease(l) {
			continue
		}
		for _, d := range snap.devices {
			if d.Namespace != l.Namespace || d.Spec.LabRef != l.Name || d.Spec.Type != lab.DeviceTypeContainer {
				continue
			}
			visible := false
			var actual *corev1.Pod
			for _, p := range snap.pods {
				if p.Namespace == d.Namespace && p.Labels[names.LabelLab] == l.Name && p.Labels[names.LabelDevice] == d.Spec.Name {
					actual = p
					if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
						visible = true
					}
				}
			}
			history := actual != nil || d.Status.PodName != "" || d.Status.NodeName != "" || d.Status.State != nil && d.Status.State.Incarnation > 0 || d.Status.Scheduling != nil && d.Status.Scheduling.State != lab.PodQueued
			dep := snap.workloads[d.Namespace+"/"+workloadName(d)]
			history = history || dep != nil
			if !history {
				continue
			} // Queued and never materialized is distinct from lost.
			node := d.Status.NodeName
			spec := corev1.PodSpec{}
			switch {
			case actual != nil:
				spec = actual.Spec
				if node == "" {
					node = actual.Spec.NodeName
				}
			case dep != nil:
				spec = dep.Spec.Template.Spec
			default:
				resources := guaranteedResources(d.Spec.Resources, s.Defaults)
				if resources == nil {
					unknown = true
					continue
				}
				spec.Containers = []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: resources}}}
			}
			need := podRequests(&corev1.Pod{Spec: spec})
			held[names.TenantOf(l.Labels)] = held[names.TenantOf(l.Labels)].Add(tenant.Totals{CPU: need.cpu, Memory: need.mem})
			if visible {
				continue
			}
			if node == "" {
				unknown = true
				continue
			}
			spec.NodeName = node
			synthetic = append(synthetic, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "held-" + d.Name, Namespace: d.Namespace}, Spec: spec})
		}
	}
	return synthetic, held, unknown
}
