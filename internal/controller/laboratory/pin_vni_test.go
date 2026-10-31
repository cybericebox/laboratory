package laboratory

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

// A Lab that is being deleted does not fail for ever on a VNI lease that has already been released (the stop of the lab retired it, or
// the quarantine has already returned it to the pool); a live Lab still gets the error.
func TestPinOwnedVNIOfAReleasedLeaseDoesNotBlockTheDeletion(t *testing.T) {
	s := runtime.NewScheme()
	if err := allocationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0) // every slot but 0 is clear: the lease of slot 7 has been returned
	pool := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: names.VNIPoolPrefix + "-0", Namespace: names.SystemNamespace, UID: "pool-uid",
			Labels: map[string]string{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}},
		Spec:   allocationv1alpha1.PoolSpec{Size: names.VNIPoolSize},
		Status: allocationv1alpha1.PoolStatus{BitMap: bitmap, Free: free},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(pool).WithObjects(pool).Build()
	r := &LabReconciler{Client: c}
	vni := uint(7)
	device := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", UID: "device-uid"}, Status: laboratoryv1alpha1.DeviceStatus{VNI: &vni}}

	err := r.pinOwnedVNI(context.Background(), device, false)
	if !errors.Is(err, poolpkg.ErrLeaseAbsent) {
		t.Fatalf("a live Lab must see the error, got %v", err)
	}
	if err := r.pinOwnedVNI(context.Background(), device, true); err != nil {
		t.Fatalf("a Lab being deleted goes on: %v", err)
	}
}
