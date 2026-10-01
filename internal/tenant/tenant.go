// Package tenant holds the pure rules of the Tenant resource shared by the management
// agent and the operator: the quota (absolute or percent) and the persistence limits.
package tenant

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Totals is a CPU (millicores) and memory (bytes) amount.
type Totals struct {
	CPU, Memory int64
}

// Add returns the sum.
func (t Totals) Add(o Totals) Totals { return Totals{t.CPU + o.CPU, t.Memory + o.Memory} }

// Limits are the resolved caps of a tenant; a Has flag false means no limit.
type Limits struct {
	HasCPU, HasMemory bool
	CPU, Memory       int64
}

// ParseLimit resolves a quota value against the allocatable total (millicores or
// bytes): an absolute quantity ("32", "500Gi", "250m"), or a percentage ("50%"). unit
// says how an absolute quantity converts: millicores for CPU, bytes for memory.
// An empty value is no limit.
func ParseLimit(v string, allocatable int64, cpu bool) (limit int64, has bool, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false, nil
	}
	if p, ok := strings.CutSuffix(v, "%"); ok {
		pct, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || pct < 0 || pct > 100 {
			return 0, false, fmt.Errorf("%q: a percentage is 0 to 100", v)
		}
		return int64(float64(allocatable) * pct / 100), true, nil
	}
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0, false, fmt.Errorf("%q: %w", v, err)
	}
	if q.Sign() < 0 {
		return 0, false, fmt.Errorf("%q is negative", v)
	}
	if cpu {
		return q.MilliValue(), true, nil
	}
	return q.Value(), true, nil
}

// ResolveQuota turns a tenant's quota into limits against the allocatable totals of the
// nodes lab pods run on. A malformed value counts as no limit for that resource (the
// admission of the Tenant is not validated deeper than the schema).
func ResolveQuota(q *laboratoryv1alpha1.TenantQuota, allocatable Totals) Limits {
	var l Limits
	if q == nil {
		return l
	}
	if v, has, err := ParseLimit(q.CPU, allocatable.CPU, true); err == nil && has {
		l.HasCPU, l.CPU = true, v
	}
	if v, has, err := ParseLimit(q.Memory, allocatable.Memory, false); err == nil && has {
		l.HasMemory, l.Memory = true, v
	}
	return l
}

// NeedsAllocatable reports whether resolving the quota needs the cluster allocatable
// (a percentage is used).
func NeedsAllocatable(q *laboratoryv1alpha1.TenantQuota) bool {
	return q != nil && (strings.HasSuffix(strings.TrimSpace(q.CPU), "%") || strings.HasSuffix(strings.TrimSpace(q.Memory), "%"))
}

// Fits reports whether adding need to reserved stays within the limits.
func (l Limits) Fits(reserved, need Totals) bool {
	if l.HasCPU && reserved.CPU+need.CPU > l.CPU {
		return false
	}
	if l.HasMemory && reserved.Memory+need.Memory > l.Memory {
		return false
	}
	return true
}

// Persistence is the effective persistence policy of a tenant.
type Persistence struct {
	// Allowed: the platform allows persistence and the tenant is permitted it.
	Allowed bool
	// WriteQuota and MaxFileSize are bytes; the platform's value is the default and the ceiling.
	WriteQuota, MaxFileSize int64
}

// EffectivePersistence combines the tenant's policy with the platform's: allowed only
// when both allow it; each limit is the tenant's value capped by the platform's, or the
// platform's when the tenant sets none (or a malformed one). A nil tenant gets the platform's policy.
func EffectivePersistence(t *laboratoryv1alpha1.Tenant, platformEnabled bool, platformQuota, platformMaxFile int64) Persistence {
	p := Persistence{Allowed: platformEnabled, WriteQuota: platformQuota, MaxFileSize: platformMaxFile}
	if t == nil {
		return p
	}
	p.Allowed = platformEnabled && t.Spec.Persistence.Allowed
	capped := func(v string, ceiling int64) int64 {
		if q, err := resource.ParseQuantity(v); err == nil && q.Value() > 0 && (ceiling <= 0 || q.Value() < ceiling) {
			return q.Value()
		}
		return ceiling
	}
	p.WriteQuota = capped(t.Spec.Persistence.WriteQuota, platformQuota)
	p.MaxFileSize = capped(t.Spec.Persistence.MaxFileSize, platformMaxFile)
	return p
}

// Quantities renders totals for the Tenant status.
func (t Totals) Usage() laboratoryv1alpha1.TenantUsage {
	return laboratoryv1alpha1.TenantUsage{
		CPU:    resource.NewMilliQuantity(t.CPU, resource.DecimalSI).String(),
		Memory: resource.NewQuantity(t.Memory, resource.BinarySI).String(),
	}
}
