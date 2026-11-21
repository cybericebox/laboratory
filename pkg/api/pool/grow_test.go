package pool

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
)

func growFixture(t *testing.T) (client.Client, *ForwardAllocator) {
	t.Helper()
	s := runtime.NewScheme()
	if err := allocationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&allocationv1alpha1.Pool{}).Build()
	bitmap, free := InitBitmap(254, 1)
	p := &allocationv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "lab-subnets-0", Namespace: "g"}, Spec: allocationv1alpha1.PoolSpec{Size: 254, Offset: 1}}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Status.BitMap, p.Status.Free = bitmap, free
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return c, &ForwardAllocator{Client: c, Prefix: "lab-subnets", Namespace: "g", Size: 254}
}

func getPool(t *testing.T, c client.Client) allocationv1alpha1.Pool {
	t.Helper()
	var p allocationv1alpha1.Pool
	if err := c.Get(context.Background(), client.ObjectKey{Name: "lab-subnets-0", Namespace: "g"}, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A pool of the old 254 numbers grows to the size of the lab network; what was handed out stays handed out, and the new numbers follow.
func TestGrowPoolKeepsWhatWasHandedOut(t *testing.T) {
	ctx := context.Background()
	c, a := growFixture(t)
	for i := 0; i < 254; i++ {
		if _, err := a.AllocateIndex(ctx); err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
	}
	if _, err := a.AllocateIndex(ctx); err == nil {
		t.Fatal("the old pool must be full")
	}
	if err := GrowPool(ctx, c, "g", "lab-subnets-0", 32767); err != nil {
		t.Fatal(err)
	}
	p := getPool(t, c)
	if p.Spec.Size != 32767 || p.Status.Free != 32767-254 {
		t.Fatalf("size %d free %d", p.Spec.Size, p.Status.Free)
	}
	n, err := a.AllocateIndex(ctx)
	if err != nil || n != 255 {
		t.Fatalf("the next number after the old pool is 255: %d, %v", n, err)
	}
}

func TestGrowPoolIsIdempotentAndNeverShrinks(t *testing.T) {
	ctx := context.Background()
	c, _ := growFixture(t)
	for _, size := range []uint{1000, 1000, 254, 100} {
		if err := GrowPool(ctx, c, "g", "lab-subnets-0", size); err != nil {
			t.Fatal(err)
		}
	}
	if p := getPool(t, c); p.Spec.Size != 1000 || p.Status.Free != 1000 {
		t.Fatalf("size %d free %d", p.Spec.Size, p.Status.Free)
	}
}

// A call that stopped between the two writes (the size is written, the free count is not) is finished by the next call.
func TestGrowPoolFinishesAnInterruptedGrowth(t *testing.T) {
	ctx := context.Background()
	c, _ := growFixture(t)
	p := getPool(t, c)
	p.Spec.Size = 600
	if err := c.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if err := GrowPool(ctx, c, "g", "lab-subnets-0", 600); err != nil {
		t.Fatal(err)
	}
	if got := getPool(t, c); got.Status.Free != 600 {
		t.Fatalf("free %d, want 600", got.Status.Free)
	}
}

func TestGrowPoolMissingPool(t *testing.T) {
	c, _ := growFixture(t)
	if err := GrowPool(context.Background(), c, "g", "nope-0", 10); err == nil {
		t.Fatal("a missing pool is an error")
	}
}
