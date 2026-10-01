package grpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	stCreated  = protobuf.ItemState_ITEM_STATE_CREATED
	stExists   = protobuf.ItemState_ITEM_STATE_EXISTS
	stUpdated  = protobuf.ItemState_ITEM_STATE_UPDATED
	stDeleted  = protobuf.ItemState_ITEM_STATE_DELETED
	stNotFound = protobuf.ItemState_ITEM_STATE_NOT_FOUND
	stFailed   = protobuf.ItemState_ITEM_STATE_FAILED
)

// wantStates asserts the states of a batch answer, in order.
func wantStates(t *testing.T, res *protobuf.BatchResult, err error, want ...protobuf.ItemState) {
	t.Helper()
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(res.GetResults()) != len(want) {
		t.Fatalf("got %d results, want %d: %v", len(res.GetResults()), len(want), res.GetResults())
	}
	for i, r := range res.GetResults() {
		if r.State != want[i] {
			t.Fatalf("result %d = %v, want %v", i, r, want[i])
		}
	}
}

func groupItems(names ...string) []*protobuf.LabGroupItem {
	out := make([]*protobuf.LabGroupItem, len(names))
	for i, n := range names {
		out[i] = &protobuf.LabGroupItem{Name: n}
	}
	return out
}

// readyGroup creates a LabGroup through the API and plays the operator: a namespace for
// it and status.namespace.
func readyGroup(t *testing.T, h *Handler, k8s kubernetes.Interface, name, ns string, labels map[string]string) {
	t.Helper()
	ctx := context.Background()
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: name, Labels: labels}}})
	wantStates(t, res, err, stCreated)
	if k8s != nil {
		mustNamespace(t, k8s, ns)
	}
	g, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g.Status.Namespace = ns
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().UpdateStatus(ctx, g, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func specJSON(devices ...string) []byte {
	spec := laboratoryv1alpha1.LabSpec{}
	spec.VPN.Enabled = true
	for _, d := range devices {
		spec.Devices = append(spec.Devices, laboratoryv1alpha1.DeviceTemplate{Name: d, Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx"})
	}
	raw, _ := json.Marshal(spec)
	return raw
}

func envOf(device string, kv ...string) *protobuf.DeviceEnv {
	vars := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		vars[kv[i]] = kv[i+1]
	}
	return &protobuf.DeviceEnv{Device: device, Vars: vars}
}

// simulateClientReconciler plays the operator for VPN clients of a namespace: it assigns
// an IP and writes a placeholder config. It stops with the test.
func simulateClientReconciler(t *testing.T, h *Handler, ns string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			list, err := h.cs.LaboratoryV1alpha1().LabGroupClients(ns).List(ctx, metav1.ListOptions{})
			if err == nil {
				for i := range list.Items {
					c := &list.Items[i]
					if c.Status.Config != "" {
						continue
					}
					c.Status.AssignedIP = "10.8.0.5/32"
					c.Status.Config = "[Interface]\nPrivateKey = " + names.WGPrivateKeyPlaceholder + "\nAddress = 10.8.0.5/32\n"
					_, _ = h.cs.LaboratoryV1alpha1().LabGroupClients(ns).UpdateStatus(ctx, c, metav1.UpdateOptions{})
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
}
