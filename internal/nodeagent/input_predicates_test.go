//go:build linux

package nodeagent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func TestConnectionDeviceInputsIgnoreSnapshotProgress(t *testing.T) {
	old := &api.Device{ObjectMeta: metav1.ObjectMeta{Name: "device", Namespace: "ns", UID: "uid"}, Spec: api.DeviceSpec{Name: "web", LabRef: "lab"}}
	updated := old.DeepCopy()
	updated.ResourceVersion = "2"
	updated.Status.State = &api.DeviceStateStatus{Epoch: 3}
	p := connectionDeviceInputs()
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
		t.Fatal("snapshot progress enqueued datapath work")
	}
	cases := map[string]func(*api.Device){
		"logical name":   func(d *api.Device) { d.Spec.Name = "new" },
		"lab membership": func(d *api.Device) { d.Spec.LabRef = "other" },
		"type":           func(d *api.Device) { d.Spec.Type = api.DeviceTypeHub },
		"node":           func(d *api.Device) { d.Status.NodeName = "n2" },
		"address":        func(d *api.Device) { d.Status.NodeAddress = "10.0.0.2" },
		"pod":            func(d *api.Device) { d.Status.PodName = "replacement" },
		"vni":            func(d *api.Device) { n := uint(42); d.Status.VNI = &n },
		"uid":            func(d *api.Device) { d.UID = "new-uid" },
		"owner":          func(d *api.Device) { d.OwnerReferences = []metav1.OwnerReference{{UID: "new-owner"}} },
		"label":          func(d *api.Device) { d.Labels = map[string]string{names.LabelLab: "other"} },
		"deleting":       func(d *api.Device) { now := metav1.Now(); d.DeletionTimestamp = &now },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			updated := old.DeepCopy()
			mutate(updated)
			if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
				t.Fatal("required device input suppressed")
			}
		})
	}
	if !p.Create(event.CreateEvent{Object: old}) || !p.Delete(event.DeleteEvent{Object: old}) {
		t.Fatal("device creation or deletion suppressed")
	}
}

func TestNetworkPodInputsPreserveLocalityAndRecovery(t *testing.T) {
	p := networkPodInputs("n1")
	old := accessPod("pod", "n1", "NET_ADMIN")
	updated := old.DeepCopy()
	updated.ResourceVersion = "new"
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
		t.Fatal("metadata-only event enqueued network work")
	}
	updated.Status.ContainerStatuses = append(updated.Status.ContainerStatuses, corev1.ContainerStatus{ContainerID: "new-sandbox"})
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
		t.Fatal("sandbox/container change suppressed")
	}
	remote := old.DeepCopy()
	remote.Spec.NodeName = "n2"
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: remote}) || !p.Update(event.UpdateEvent{ObjectOld: remote, ObjectNew: old}) {
		t.Fatal("old/new node membership transition suppressed")
	}
	remoteUpdated := remote.DeepCopy()
	remoteUpdated.Annotations[names.AnnotationNetworks] = "eth1@"
	if p.Update(event.UpdateEvent{ObjectOld: remote, ObjectNew: remoteUpdated}) {
		t.Fatal("remote-only event enqueued local networking")
	}
	for _, mutate := range []func(){
		func() { updated = old.DeepCopy(); updated.UID = "new" },
		func() { updated = old.DeepCopy(); updated.OwnerReferences = []metav1.OwnerReference{{UID: "owner"}} },
		func() { updated = old.DeepCopy(); delete(updated.Labels, names.LabelLab) },
		func() { updated = old.DeepCopy(); updated.Annotations[names.AnnotationNetworks] = "eth1@" },
		func() { updated = old.DeepCopy(); now := metav1.Now(); updated.DeletionTimestamp = &now },
	} {
		mutate()
		if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
			t.Fatal("required Pod identity/attachment/deletion input suppressed")
		}
	}
	if !p.Create(event.CreateEvent{Object: remote}) || !p.Delete(event.DeleteEvent{Object: remote}) {
		t.Fatal("creation/deletion contract changed")
	}
}
