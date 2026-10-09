//go:build linux

package reconciler

import (
	"context"
	"fmt"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestControlTwentyPeersAndLabsIgnoreOneHundredStatsUpdates(t *testing.T) {
	r, applier, c, req := accessReconcileFixture(t)
	for i := 2; i <= 20; i++ {
		p := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Namespace: "group", Name: fmt.Sprintf("p%d", i)}, Status: lab.LabGroupClientStatus{AssignedIP: fmt.Sprintf("10.8.0.%d/32", i+1)}}
		l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Namespace: "group", Name: fmt.Sprintf("l%d", i)}, Status: lab.LabStatus{Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true, CIDR: fmt.Sprintf("10.8.%d.0/24", i)}}}
		if err := c.Create(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(context.Background(), l); err != nil {
			t.Fatal(err)
		}
	}
	reconcileAccess(t, r, req)
	if len(applier.active) != 400 {
		t.Fatalf("decision matrix %d", len(applier.active))
	}
	for i := 0; i < 100; i++ {
		changePeer(t, c, func(p *lab.LabGroupClient) { p.Status.Statistics.RxBytes++ })
		reconcileAccess(t, r, req)
	}
	if applier.applications != 1 {
		t.Fatalf("stats applied %d times", applier.applications)
	}
	policy := &lab.LabGroupAccessPolicy{}
	key := types.NamespacedName{Namespace: "group", Name: req.Name}
	if err := c.Get(context.Background(), key, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.Rules = []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow, ClientNames: []string{"p1"}, LabNames: []string{"l1"}}}
	if err := c.Update(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	reconcileAccess(t, r, req)
	if applier.applications != 2 {
		t.Fatalf("permission change applies %d", applier.applications)
	}
}
