package laboratory

import (
	"context"

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

// syncConsoleCables makes the ConsoleCables of the spec and removes the ones it no longer has.
func (r *LabReconciler) syncConsoleCables(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	if err := r.materializeConsoleCables(ctx, lab); err != nil {
		return err
	}
	return r.pruneConsoleCables(ctx, lab)
}

// materializeConsoleCables creates the ConsoleCable of every cable of the spec that has none.
func (r *LabReconciler) materializeConsoleCables(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	for _, tmpl := range lab.Spec.ConsoleCables {
		name := consoleCableName(lab.Name, tmpl)
		var existing laboratoryv1alpha1.ConsoleCable
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lab.Namespace}, &existing)
		if err == nil {
			continue
		}
		if !errors.IsNotFound(err) {
			return err
		}
		cc := &laboratoryv1alpha1.ConsoleCable{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lab.Namespace, Labels: deviceLabels(lab)},
			Spec:       laboratoryv1alpha1.ConsoleCableSpec{LabRef: lab.Name, Serial: tmpl.Serial, Console: tmpl.Console},
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
	var list laboratoryv1alpha1.ConsoleCableList
	if err := r.List(ctx, &list, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
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
