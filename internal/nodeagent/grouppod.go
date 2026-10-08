package nodeagent

import (
	"context"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/netattach"
)

// maxLabIndex is the largest lab network index of a group (the lab-subnets pool is 1..254).
const maxLabIndex = 254

// GroupComponent says which of a group's own pods this is: names.ComponentVPN, names.ComponentGateway, or "" for any other pod
// (a device pod). A pod made before the component label is told by `app`.
func GroupComponent(pod *corev1.Pod) string {
	if c := pod.Labels[names.LabelComponent]; c == names.ComponentVPN || c == names.ComponentGateway {
		return c
	}
	if _, device := pod.Labels[names.LabelLab]; device {
		return ""
	}
	if app := pod.Labels["app"]; app == names.ComponentVPN || app == names.ComponentGateway {
		return app
	}
	return ""
}

// groupPortKey is the host-side OVS port of lab n's leg in the VPN or gateway pod of a group namespace.
func groupPortKey(component, namespace string, n uint) string {
	if component == names.ComponentGateway {
		return names.GWHostPortKey(namespace, n)
	}
	return names.VPNHostPortKey(namespace, n)
}

// GroupPodAttachments is what the VPN or gateway pod of a group needs wired: one interface lab{N} per lab network, taken from the
// group's LabVPN or LabGateway objects (the operator makes one per lab with a VPN or an internet leg, and the object lives until
// the VPN or gateway process has cleaned up after the lab). The pod template carries no list of them, so adding or removing a lab never
// changes the Deployment and never restarts the pod; the node-agent attaches and detaches the interfaces in the running pod.
func GroupPodAttachments(ctx context.Context, c client.Reader, namespace, component string) ([]netattach.Attachment, error) {
	var indexes []uint
	switch component {
	case names.ComponentVPN:
		var list laboratoryv1alpha1.LabVPNList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range list.Items {
			active, err := labRuntimeActive(ctx, c, namespace, list.Items[i].Spec.LabName)
			if err != nil {
				return nil, err
			}
			if active && !list.Items[i].DeletionTimestamp.IsZero() {
				active = false
			}
			if active {
				indexes = append(indexes, list.Items[i].Spec.NetworkIndex)
			}
		}
	case names.ComponentGateway:
		var list laboratoryv1alpha1.LabGatewayList
		if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range list.Items {
			active, err := labRuntimeActive(ctx, c, namespace, list.Items[i].Spec.LabName)
			if err != nil {
				return nil, err
			}
			if active && !list.Items[i].DeletionTimestamp.IsZero() {
				active = false
			}
			if active {
				indexes = append(indexes, list.Items[i].Spec.NetworkIndex)
			}
		}
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	var out []netattach.Attachment
	seen := map[uint]bool{}
	for _, n := range indexes {
		if n == 0 || n > maxLabIndex || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, netattach.Attachment{Iface: names.LabIfaceNameByIndex(n), Name: groupPortKey(component, namespace, n)})
	}
	return out, nil
}

// StaleGroupPorts returns the ports among present that belong to the group's VPN or gateway pod (any lab index) and are not in wanted:
// the legs of labs that are gone, to be detached from the running pod.
func StaleGroupPorts(namespace, component string, wanted []netattach.Attachment, present map[string]bool) []string {
	keep := map[string]bool{}
	for _, a := range wanted {
		keep[a.Name] = true
	}
	var stale []string
	for n := uint(1); n <= maxLabIndex; n++ {
		if key := groupPortKey(component, namespace, n); present[key] && !keep[key] {
			stale = append(stale, key)
		}
	}
	return stale
}

// GroupPortsPresent returns the ports among present that belong to the group's pod of this component (any lab index).
func GroupPortsPresent(namespace, component string, present map[string]bool) []string {
	var out []string
	for n := uint(1); n <= maxLabIndex; n++ {
		if key := groupPortKey(component, namespace, n); present[key] {
			out = append(out, key)
		}
	}
	return out
}

// Runtime inputs are fail closed: missing parent intent cannot authorize wiring.
func labRuntimeActive(ctx context.Context, c client.Reader, namespace, name string) (bool, error) {
	var l laboratoryv1alpha1.Lab
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &l); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return l.DeletionTimestamp.IsZero() && !l.Spec.Lifecycle.IsStopped(), nil
}
