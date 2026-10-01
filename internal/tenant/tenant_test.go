package tenant

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestResolveQuota(t *testing.T) {
	alloc := Totals{CPU: 16000, Memory: 64 << 30}
	l := ResolveQuota(&laboratoryv1alpha1.TenantQuota{CPU: "50%", Memory: "8Gi"}, alloc)
	if !l.HasCPU || l.CPU != 8000 || !l.HasMemory || l.Memory != 8<<30 {
		t.Fatalf("%+v", l)
	}
	if l := ResolveQuota(nil, alloc); l.HasCPU || l.HasMemory {
		t.Fatal("no quota, no limit")
	}
	if l := ResolveQuota(&laboratoryv1alpha1.TenantQuota{CPU: "2500m"}, alloc); l.CPU != 2500 || l.HasMemory {
		t.Fatalf("absolute cpu: %+v", l)
	}
	for _, bad := range []string{"150%", "-1", "lots", "x%"} {
		if l := ResolveQuota(&laboratoryv1alpha1.TenantQuota{CPU: bad}, alloc); l.HasCPU {
			t.Errorf("%q must not become a limit", bad)
		}
	}
	if !NeedsAllocatable(&laboratoryv1alpha1.TenantQuota{Memory: "10%"}) || NeedsAllocatable(&laboratoryv1alpha1.TenantQuota{CPU: "2"}) || NeedsAllocatable(nil) {
		t.Fatal("percent needs the allocatable")
	}
}

func TestFits(t *testing.T) {
	l := Limits{HasCPU: true, CPU: 1000, HasMemory: true, Memory: 100}
	if !l.Fits(Totals{600, 50}, Totals{400, 50}) || l.Fits(Totals{600, 50}, Totals{401, 0}) || l.Fits(Totals{0, 60}, Totals{0, 41}) {
		t.Fatal("limits")
	}
	if !(Limits{}).Fits(Totals{1 << 40, 1 << 40}, Totals{1, 1}) {
		t.Fatal("no limit")
	}
}

func TestEffectivePersistence(t *testing.T) {
	const mi = 1 << 20
	ten := func(allowed bool, wq, mf string) *laboratoryv1alpha1.Tenant {
		return &laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "t"}, Spec: laboratoryv1alpha1.TenantSpec{
			Persistence: laboratoryv1alpha1.TenantPersistence{Allowed: allowed, WriteQuota: wq, MaxFileSize: mf}}}
	}
	if p := EffectivePersistence(nil, true, 512*mi, 256*mi); !p.Allowed || p.WriteQuota != 512*mi {
		t.Fatalf("no tenant: %+v", p)
	}
	if p := EffectivePersistence(ten(false, "", ""), true, 512*mi, 256*mi); p.Allowed {
		t.Fatal("the tenant does not allow it")
	}
	if p := EffectivePersistence(ten(true, "", ""), false, 512*mi, 256*mi); p.Allowed {
		t.Fatal("the platform does not allow it")
	}
	p := EffectivePersistence(ten(true, "100Mi", "1Gi"), true, 512*mi, 256*mi)
	if !p.Allowed || p.WriteQuota != 100*mi || p.MaxFileSize != 256*mi {
		t.Fatalf("lower wins, ceiling caps: %+v", p)
	}
	if p := EffectivePersistence(ten(true, "junk", "0"), true, 512*mi, 256*mi); p.WriteQuota != 512*mi || p.MaxFileSize != 256*mi {
		t.Fatalf("malformed values fall back to the platform: %+v", p)
	}
}
