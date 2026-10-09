package laboratory

import (
	"context"
	"fmt"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Configured membership is independent of readiness. Positive placements and
// unresolved UID/boot-bound history widen obligations, never certify absence.
type nativePlacementPolicy struct {
	Selector      map[string]string
	Tolerations   []corev1.Toleration
	History       []lab.OwnedRuntimeIdentity
	Reports       []lab.OwnedRuntimeReport
	PositiveNodes map[string]bool
}

func configuredNativePlacement(node *corev1.Node, p nativePlacementPolicy) bool {
	selector := map[string]string{}
	for key, value := range p.Selector {
		if key != names.LabelNodeAgentReady {
			selector[key] = value
		}
	}
	copy := node.DeepCopy()
	copy.Spec.Unschedulable = false
	copy.Status.Conditions = nil
	copy.Spec.Taints = nil
	for _, taint := range node.Spec.Taints {
		switch taint.Key {
		case corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable, corev1.TaintNodeUnschedulable, corev1.TaintNodeMemoryPressure, corev1.TaintNodeDiskPressure, corev1.TaintNodePIDPressure, corev1.TaintNodeNetworkUnavailable, "node.cilium.io/agent-not-ready":
			// These express temporary availability/bootstrap, not pool membership.
			continue
		}
		copy.Spec.Taints = append(copy.Spec.Taints, taint)
	}
	return nodecap.Schedulable(copy, selector, p.Tolerations)
}
func (p nativePlacementPolicy) requiredHistoricalNodes(owner, namespace string, nodes map[string]*corev1.Node) (map[string]bool, error) {
	required := map[string]bool{}
	for name := range p.PositiveNodes {
		required[name] = true
	}
	for _, row := range p.History {
		if row.OwnerUID != owner || row.NodeName == "" {
			continue
		}
		if runtimeRowsReleased([]lab.OwnedRuntimeIdentity{row}, p.Reports, row.OwnerUID, row.OperationID, row.Revision) {
			continue
		}
		// An old node name alone may not be rebound to a replacement boot.
		if row.NodeBootID == "" || row.Namespace != namespace || row.ScopeUID == "" && row.PodUID == "" {
			return nil, fmt.Errorf("native placement history identity incomplete")
		}
		node := nodes[row.NodeName]
		if node == nil || node.Status.NodeInfo.BootID != row.NodeBootID {
			return nil, fmt.Errorf("original native placement node unavailable or replaced")
		}
		required[row.NodeName] = true
	}
	for name := range required {
		if nodes[name] == nil {
			return nil, fmt.Errorf("positive native placement node unavailable")
		}
	}
	return required, nil
}
func reportHistory(reports []lab.OwnedRuntimeReport) []lab.OwnedRuntimeIdentity {
	rows := make([]lab.OwnedRuntimeIdentity, 0, len(reports))
	for _, report := range reports {
		rows = append(rows, report.Identity)
	}
	return rows
}
func (r *LabReconciler) labNativePlacement(ctx context.Context, l *lab.Lab, devices []lab.Device, connections []lab.Connection) (nativePlacementPolicy, error) {
	p := nativePlacementPolicy{Selector: r.LabNodeSelector, Tolerations: r.LabTolerations, History: adoptReleasedScopeHistory(l.Status.ScopeInventory, l.Status.ScopeReports), Reports: append([]lab.OwnedRuntimeReport(nil), l.Status.ScopeReports...), PositiveNodes: map[string]bool{}}
	p.History = append(p.History, reportHistory(l.Status.ScopeReports)...)
	owned := []*lab.Device{}
	for i := range devices {
		d := &devices[i]
		if !objectOwnedBy(d, string(l.UID)) {
			continue
		}
		owned = append(owned, d)
		if d.Status.NodeName != "" {
			p.PositiveNodes[d.Status.NodeName] = true
		}
		for _, row := range d.Status.RuntimeInventory {
			if devicePlacementIdentity(l, d, row) {
				p.History = append(p.History, row)
			}
		}
		for _, report := range d.Status.RuntimeReports {
			if devicePlacementIdentity(l, d, report.Identity) {
				p.History = append(p.History, report.Identity)
				p.Reports = append(p.Reports, report)
			}
		}
	}
	for _, conn := range connections {
		if objectOwnedBy(&conn, string(l.UID)) {
			for _, port := range conn.Status.Ports {
				if port.NodeName != "" {
					p.PositiveNodes[port.NodeName] = true
				}
			}
		}
	}
	var pods corev1.PodList
	if err := r.lifecycleReader().List(ctx, &pods, client.InNamespace(l.Namespace)); err != nil {
		return p, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" {
			continue
		}
		for _, d := range owned {
			matches, err := r.ownedRuntimeDevicePod(ctx, d, pod)
			if err != nil {
				return p, err
			}
			if matches {
				p.PositiveNodes[pod.Spec.NodeName] = true
				break
			}
		}
	}
	return p, nil
}
func (r *LabGroupReconciler) groupNativePlacement(ctx context.Context, g *lab.LabGroup, pods []corev1.Pod) (nativePlacementPolicy, error) {
	p := nativePlacementPolicy{Selector: r.LabNodeSelector, Tolerations: r.LabTolerations, History: adoptReleasedScopeHistory(g.Status.ServiceRuntime, g.Status.ServiceReports), Reports: g.Status.ServiceReports, PositiveNodes: map[string]bool{}}
	p.History = append(p.History, reportHistory(g.Status.ServiceReports)...)
	ns := lab.LabGroupNamespaceOf(g)
	for i := range pods {
		pod := &pods[i]
		component := serviceComponent(pod)
		if component == "" || pod.Spec.NodeName == "" {
			continue
		}
		var dep appsv1.Deployment
		if err := r.groupReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: component}, &dep); err != nil {
			return p, err
		}
		if !r.servicePodOwned(ctx, pod, &dep) {
			continue
		}
		if err := checkServiceGroupUID(&dep, string(g.UID)); err != nil {
			return p, err
		}
		p.PositiveNodes[pod.Spec.NodeName] = true
	}
	return p, nil
}

func devicePlacementIdentity(l *lab.Lab, d *lab.Device, row lab.OwnedRuntimeIdentity) bool {
	if row.OwnerUID != string(l.UID) || row.Namespace != "" && row.Namespace != d.Namespace {
		return false
	}
	if row.ScopeUID != "" {
		return row.ScopeUID == string(d.UID)
	}
	// Earlier producer rows used the Lab UID and exact Pod UID before ScopeUID.
	// Keep their unresolved debt; incomplete namespace/boot remains Unknown.
	return row.PodUID != ""
}
