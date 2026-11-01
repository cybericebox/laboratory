//go:build linux

package nodeagent

import (
	"context"
	"testing"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A Device whose lease is no longer held (its slot went back or to another owner) claims no VNI domain and does not fail the scan, but
// the flows of that VNI are not forgiven: a flow with the VNI stays unattributed, so the scan stays fail-closed until the flows are
// dropped by the release of the owner (and the sweeper keeps the slot from a new owner until that acknowledgement).
func TestLostLeaseClaimsNoDomainAndItsFlowsStayUnattributed(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	ctx := context.Background()
	c, ok := o.Reader.(client.Client)
	if !ok {
		t.Fatal("test reader is not a full client")
	}
	scheme := c.Scheme()
	_ = allocation.AddToScheme(scheme)
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	pool := &allocation.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool-uid", Labels: map[string]string{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}},
		Spec:       allocation.PoolSpec{Size: names.VNIPoolSize},
		Status:     allocation.PoolStatus{BitMap: bitmap, Free: free},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	vni := uint(901)
	known := map[uint64]bool{}
	if err := o.claimLeaseDomain(ctx, known, "dev", &vni, &lab.VNILease{PoolUID: "pool-uid", Generation: 3}); err != nil {
		t.Fatalf("a lost lease must not fail the scan: %v", err)
	}
	if known[901] {
		t.Fatal("a lost lease claimed its VNI domain")
	}
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return []nativeFlow{{VNI: 901, HasVNI: true, Raw: "table=6,metadata=0x385 actions=output:7"}}, nil
	}
	if err := o.verifyNativeFlowAttachments(ctx, &id, &lab.ConnectionList{}, nil, nil, false); err == nil {
		t.Fatal("a flow of a VNI nobody holds was accepted")
	}
}
