package laboratory

import (
	"context"
	"encoding/json"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// Console cables, the operator side (docs/internal-docs/laboratory/specs/console-ports-design.md, 6.1). One ConsoleCable per entry of
// spec.consoleCables, owned by the Lab. A cable is never a Connection: nothing of it reaches Open vSwitch, a VNI or the node-agent's
// connection reconciler. The line itself (attach, relay, status) is driven by the node-agent of the console end and the device access
// service; the operator only makes and removes the objects and sums their state into the Lab status.

// consoleCableName is the name of the ConsoleCable of a cable: deterministic, bounded, and different when either end changes (the
// object is immutable, so a re-plugged cable is a new object).
func consoleCableName(lab string, c laboratoryv1alpha1.ConsoleCableTemplate) string {
	return connectionName(lab, []laboratoryv1alpha1.EndpointSpec{
		{Device: c.Serial.Device, Interface: c.Serial.Port},
		{Device: c.Console.Device, Interface: c.Console.Port},
	})
}

// materializeLinks makes the Connections of the spec, then its ConsoleCables (and removes the cables the spec no longer has).
func (r *LabReconciler) materializeLinks(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	if err := r.materializeConnections(ctx, lab); err != nil {
		return err
	}
	return r.syncConsoleCables(ctx, lab)
}

// consoleCablesOn says the laboratory serves console lines (consoleCables.enabled; nil switches: everything). Only then does the operator
// watch ConsoleCables, so an upgrade with the switch off needs no new CRD to keep the labs running.
func (r *LabReconciler) consoleCablesOn() bool { return r.Switches == nil || r.Switches.ConsoleCables }

// noCableKind says the API server does not know the ConsoleCable kind (the CRDs of this release were not applied: helm upgrade does not
// apply crds/). A lab then has no cable; a lab that declares one is refused by the switch or waits for the CRD.
func noCableKind(err error) bool { return meta.IsNoMatchError(err) }

// syncConsoleCables makes the ConsoleCables of the spec and removes the ones it no longer has. A lab without cables on a laboratory with
// the switch off makes no ConsoleCable call at all.
func (r *LabReconciler) syncConsoleCables(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	if len(lab.Spec.ConsoleCables) == 0 && !r.consoleCablesOn() {
		return nil
	}
	if err := r.materializeConsoleCables(ctx, lab); err != nil {
		return err
	}
	if err := r.pruneConsoleCables(ctx, lab); err != nil && !noCableKind(err) {
		return err
	}
	return nil
}

// consoleNodes is the node of every device of the lab (Device.status.nodeName), by the device's name in the lab.
func (r *LabReconciler) consoleNodes(ctx context.Context, lab *laboratoryv1alpha1.Lab) (map[string]string, error) {
	var list laboratoryv1alpha1.DeviceList
	if err := r.List(ctx, &list, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range list.Items {
		d := &list.Items[i]
		if d.Spec.LabRef == lab.Name {
			out[d.Spec.Name] = d.Status.NodeName
		}
	}
	return out, nil
}

// materializeConsoleCables creates the ConsoleCable of every cable of the spec that has none, and keeps the annotation
// names.AnnotationConsoleNode of every cable equal to the node of its console end: the admission policy of the node-agent lets only the
// node-agent of that node write the cable's status.
func (r *LabReconciler) materializeConsoleCables(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	if len(lab.Spec.ConsoleCables) == 0 {
		return nil
	}
	nodes, err := r.consoleNodes(ctx, lab)
	if err != nil {
		return err
	}
	for _, tmpl := range lab.Spec.ConsoleCables {
		name := consoleCableName(lab.Name, tmpl)
		node := nodes[tmpl.Console.Device]
		var existing laboratoryv1alpha1.ConsoleCable
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lab.Namespace}, &existing)
		if err == nil {
			if err := r.stampConsoleNode(ctx, &existing, node); err != nil {
				return err
			}
			continue
		}
		if !errors.IsNotFound(err) {
			return err
		}
		cc := &laboratoryv1alpha1.ConsoleCable{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lab.Namespace, Labels: deviceLabels(lab)},
			Spec:       laboratoryv1alpha1.ConsoleCableSpec{LabRef: lab.Name, Serial: tmpl.Serial, Console: tmpl.Console},
		}
		if node != "" {
			cc.Annotations = map[string]string{names.AnnotationConsoleNode: node}
		}
		if err := controllerutil.SetOwnerReference(lab, cc, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, cc); err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// stampConsoleNode puts the node of the console end on the cable (a merge patch of that annotation only; none while the device has no node).
func (r *LabReconciler) stampConsoleNode(ctx context.Context, c *laboratoryv1alpha1.ConsoleCable, node string) error {
	if c.Annotations[names.AnnotationConsoleNode] == node {
		return nil
	}
	var value any
	if node != "" {
		value = node
	}
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{names.AnnotationConsoleNode: value}}})
	return client.IgnoreNotFound(r.Patch(ctx, c, client.RawPatch(types.MergePatchType, body)))
}

// pruneConsoleCables deletes the ConsoleCables of the lab that its spec no longer has (an unplugged or re-plugged cable).
func (r *LabReconciler) pruneConsoleCables(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	desired := make(map[string]bool, len(lab.Spec.ConsoleCables))
	for _, c := range lab.Spec.ConsoleCables {
		desired[consoleCableName(lab.Name, c)] = true
	}
	var list laboratoryv1alpha1.ConsoleCableList
	if err := r.List(ctx, &list, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
		return err
	}
	for i := range list.Items {
		c := &list.Items[i]
		if desired[c.Name] || !c.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, c); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// consoleCableRefs is the summary of the lab's ConsoleCables for its status. A line that is down never holds the lab back from Ready:
// a device being Ready does not wait for its lines (6.5), and a cable is not a connection of the lab network.
func (r *LabReconciler) consoleCableRefs(ctx context.Context, lab *laboratoryv1alpha1.Lab) ([]laboratoryv1alpha1.ConsoleCableRef, error) {
	if len(lab.Spec.ConsoleCables) == 0 && len(lab.Status.ConsoleCables) == 0 {
		return nil, nil
	}
	if len(lab.Spec.ConsoleCables) == 0 && !r.consoleCablesOn() {
		return nil, nil
	}
	var list laboratoryv1alpha1.ConsoleCableList
	if err := r.List(ctx, &list, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
		if noCableKind(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []laboratoryv1alpha1.ConsoleCableRef
	for i := range list.Items {
		c := &list.Items[i]
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		ref := laboratoryv1alpha1.ConsoleCableRef{Name: c.Name, Serial: c.Spec.Serial, Console: c.Spec.Console, LineUp: c.Status.LineUp}
		if !ref.LineUp {
			ref.Reason = laboratoryv1alpha1.ConsoleCableReasonWaitingForDevice
			if cond := meta.FindStatusCondition(c.Status.Conditions, labstatus.ConditionReady); cond != nil && cond.Reason != "" {
				ref.Reason = cond.Reason
			}
		}
		out = append(out, ref)
	}
	return out, nil
}
