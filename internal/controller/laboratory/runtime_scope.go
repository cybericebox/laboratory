package laboratory

import (
	"context"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Runtime scopes cover every actual registered placement node, without invented
// Pod/container identities. Old node obligations remain separately held.
func declaredScopes(ctx context.Context, reader client.Reader, owner, namespace, name, op string, revision, generation int64, kind string) ([]lab.OwnedRuntimeIdentity, error) {
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes, client.MatchingLabels{names.LabelNodeAgentReady: "true"}); err != nil {
		return nil, err
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("native placement node inventory unavailable")
	}
	var scopes []lab.OwnedRuntimeIdentity
	for _, node := range nodes.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			ready = ready || condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue
		}
		if !ready || node.Status.NodeInfo.BootID == "" {
			return nil, fmt.Errorf("native placement node unavailable")
		}
		scopes = append(scopes, lab.OwnedRuntimeIdentity{ScopeKind: kind, ScopeUID: owner, OwnerUID: owner, Namespace: namespace, LabName: name, OperationID: op, Revision: revision, Generation: generation, NodeName: node.Name, NodeBootID: node.Status.NodeInfo.BootID})
	}
	return scopes, nil
}
func (r *LabReconciler) prepareLabScopes(ctx context.Context, l *lab.Lab) error {
	if !r.RuntimeObservation {
		return nil
	}
	i := l.Spec.Lifecycle
	declared, err := declaredScopes(ctx, r.lifecycleReader(), string(l.UID), l.Namespace, l.Name, i.OperationID, i.Revision, l.Generation, "LabFabric")
	if err != nil {
		return err
	}
	var devices lab.DeviceList
	if err := r.lifecycleReader().List(ctx, &devices, client.InNamespace(l.Namespace)); err != nil {
		return err
	}
	var connections lab.ConnectionList
	if err := r.lifecycleReader().List(ctx, &connections, client.InNamespace(l.Namespace)); err != nil {
		return err
	}
	bindings := []lab.OwnedVNI{}
	for _, d := range devices.Items {
		if objectOwnedBy(&d, string(l.UID)) && d.Status.VNI != nil {
			bindings = append(bindings, lab.OwnedVNI{Kind: "Device", Namespace: d.Namespace, Name: d.Name, UID: string(d.UID), VNI: *d.Status.VNI})
		}
	}
	for _, c := range connections.Items {
		if objectOwnedBy(&c, string(l.UID)) && c.Status.VNI != nil {
			bindings = append(bindings, lab.OwnedVNI{Kind: "Connection", Namespace: c.Namespace, Name: c.Name, UID: string(c.UID), VNI: *c.Status.VNI})
		}
	}
	for n := range declared {
		declared[n].VNIBindings = bindings
		for _, binding := range bindings {
			declared[n].VNIs = append(declared[n].VNIs, binding.VNI)
		}
	}
	rows := append([]lab.OwnedRuntimeIdentity(nil), l.Status.ScopeInventory...)
	for _, scope := range declared {
		for _, report := range l.Status.ScopeReports {
			if sameDeclaredScope(scope, report.Identity) && report.Identity.AttachmentsComplete {
				scope = report.Identity
				break
			}
		}
		found := false
		for n, old := range rows {
			if sameDeclaredScope(scope, old) {
				rows[n] = scope
				found = true
				break
			}
		}
		if !found {
			rows = append(rows, scope)
		}
	}
	if reflect.DeepEqual(rows, l.Status.ScopeInventory) {
		return nil
	}
	base := l.DeepCopy()
	l.Status.ScopeInventory = rows
	return r.Status().Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
func sameDeclaredScope(a, b lab.OwnedRuntimeIdentity) bool {
	return a.ScopeKind == b.ScopeKind && a.ScopeUID == b.ScopeUID && a.OwnerUID == b.OwnerUID && a.NodeName == b.NodeName && a.NodeBootID == b.NodeBootID && a.Generation == b.Generation && a.OperationID == b.OperationID && a.Revision == b.Revision
}
