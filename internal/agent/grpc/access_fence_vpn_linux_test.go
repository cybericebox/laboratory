//go:build linux

package grpc

import (
	"context"
	"errors"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
	"github.com/cybericebox/laboratory/internal/vpn/reconciler"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type publicationACLKernel struct{ calls int }

func (k *publicationACLKernel) ReplaceAccessRules([]vpn.AccessRule) error { k.calls++; return nil }
func (*publicationACLKernel) AccessCounters() (map[string]vpn.TrafficCounter, error) {
	return map[string]vpn.TrafficCounter{}, nil
}

type publicationACLRevoker struct {
	calls int
	fail  bool
}

func (r *publicationACLRevoker) Revoke([]string, []vpn.AccessRule, ...[]string) (int, error) {
	r.calls++
	if r.fail {
		return 0, errors.New("conntrack unavailable")
	}
	return 0, nil
}

// The setter, persisted Spec, real VPN Reconcile/status writer and converter are
// exercised together. Only the external packet filter/conntrack are doubles.
func TestAccessFenceVPNStoredTupleReachesCurrentBootACK(t *testing.T) {
	for _, mode := range []string{"current", "wrong group", "conntrack failure"} {
		t.Run(mode, func(t *testing.T) {
			h, cs, input := accessFencePublicationRig()
			if accessFenceState(t, h, input) != stCreated {
				t.Fatal("exported setter failed")
			}
			stored, err := cs.LaboratoryV1alpha1().LabGroupAccessPolicies("group-ns").Get(context.Background(), names.LabGroupAccessPolicyName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			// Explicit API-server metadata for the typed fake -> controller adapter.
			stored.UID = "policy-uid"
			stored.Generation = 1
			stored.ResourceVersion = ""
			scheme := runtime.NewScheme()
			_ = lab.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)
			identity := lab.VPNRuntimeIdentity{BootID: "current-vpn-boot", PodName: "vpn-pod", PodUID: "vpn-pod-uid", ContainerID: "containerd://vpn-current"}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: identity.PodName, Namespace: "group-ns", UID: "vpn-pod-uid", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: names.ComponentVPN, ContainerID: identity.ContainerID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
			report := &lab.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: flowacct.ReportName, Namespace: "group-ns"}, Spec: lab.LabTrafficReportSpec{Kind: lab.LabTrafficSurfaceVPN, Instance: identity.PodName}, Status: lab.LabTrafficReportStatus{CurrentVPNRuntime: &lab.VPNBootRecord{VPNRuntimeIdentity: identity, GroupUID: "group-uid", PublishedAt: metav1.Now()}}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&lab.LabGroupAccessPolicy{}, &lab.LabTrafficReport{}).WithObjects(stored, pod, report).Build()
			kernel := &publicationACLKernel{}
			revoker := &publicationACLRevoker{fail: mode == "conntrack failure"}
			r := &reconciler.AccessReconciler{Client: c, Reader: c, IPT: kernel, Conntrack: revoker, GroupUID: "group-uid", Runtime: identity}
			if mode == "wrong group" {
				r.GroupUID = "foreign-group"
				report.Status.CurrentVPNRuntime.GroupUID = r.GroupUID
				if err := c.Status().Update(context.Background(), report); err != nil {
					t.Fatal(err)
				}
			}
			_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(stored)})
			if mode == "current" && err != nil {
				t.Fatal(err)
			}
			if mode != "current" && err == nil {
				t.Fatal("invalid physical ACK accepted")
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(stored), stored); err != nil {
				t.Fatal(err)
			}
			projected := accessPolicyToProto(stored, input.LabGroupName)
			if mode == "current" {
				if projected.Status.State != "Applied" || projected.Status.AppliedRevision != 3 || projected.Status.OperationId != input.OperationId || projected.Status.VpnBootId != identity.BootID || projected.Status.ObservedGeneration != stored.Generation || kernel.calls != 1 || revoker.calls != 1 {
					t.Fatalf("existing VPN ACK lost setter tuple: %v apply=%d revoke=%d", projected, kernel.calls, revoker.calls)
				}
			} else if projected.Status.State == "Applied" || projected.Status.AppliedRevision != 0 {
				t.Fatalf("foreign/failed physical ACK published: %v", projected)
			}
		})
	}
}
