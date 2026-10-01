package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantSpec is the policy of one tenant of the management agent. The tenant is
// identified by the Tenant's name: the client certificate issued for it carries
// CN = name, and the agent resolves the caller's tenant by that CN.
type TenantSpec struct {
	// Persistence is what the tenant may do with device state persistence.
	// +optional
	Persistence TenantPersistence `json:"persistence,omitempty"`
	// Quota caps the CPU and memory requests of the tenant's running pods.
	// Absent: no limit.
	// +optional
	Quota *TenantQuota `json:"quota,omitempty"`
}

// TenantPersistence is the persistence policy of a tenant. The platform switch
// (chart devices.statePersistence) allows the mechanism at all, and its values are
// the ceilings of the limits below.
type TenantPersistence struct {
	// Allowed lets the tenant's topologies ask for persistence (devices[].persistence.enabled).
	// It has no effect while the platform does not allow persistence.
	// +optional
	Allowed bool `json:"allowed,omitempty"`
	// WriteQuota is the most of a participant's writes kept per device (a Kubernetes
	// quantity); capped by the platform's value, which is also the default.
	// +optional
	WriteQuota string `json:"writeQuota,omitempty"`
	// MaxFileSize: a file larger than this is not snapshotted (a Kubernetes quantity);
	// capped by the platform's value, which is also the default.
	// +optional
	MaxFileSize string `json:"maxFileSize,omitempty"`
}

// TenantQuota caps the sum of the CPU and memory requests of the tenant's dispatched
// pods. Each limit is an absolute quantity ("32", "500Gi") or a percentage of what
// the nodes lab pods can run on allocate ("50%"). Absent: no limit.
type TenantQuota struct {
	// +optional
	CPU string `json:"cpu,omitempty"`
	// +optional
	Memory string `json:"memory,omitempty"`
}

// TenantUsage is a CPU and memory total as Kubernetes quantities.
type TenantUsage struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// TenantStatus is the observed load of the tenant, refreshed by the management agent.
type TenantStatus struct {
	// Reserved is the sum of the requests of the tenant's pods.
	// +optional
	Reserved TenantUsage `json:"reserved,omitempty"`
	// Used is the live consumption of the tenant's pods (metrics-server); empty
	// while metrics are not available.
	// +optional
	Used TenantUsage `json:"used,omitempty"`
	// ObservedAt is when Reserved and Used were taken.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// Enrollment is the one-time token a client enrolls with (see the agent's Enroll).
	// +optional
	Enrollment *TenantEnrollment `json:"enrollment,omitempty"`
}

// TenantEnrollment holds the state of the tenant's enrollment token. Only its hash is
// stored; the token itself is in the Secret tenant-<name>-enrollment of the tenants namespace
// until it is used or replaced.
type TenantEnrollment struct {
	// TokenHash is the hex SHA-256 of the token.
	TokenHash string `json:"tokenHash,omitempty"`
	// IssuedAt is when the token was generated.
	IssuedAt *metav1.Time `json:"issuedAt,omitempty"`
	// ExpiresAt is when an unused token stops working.
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// UsedAt is when the token was used to enroll; a used token never works again.
	UsedAt *metav1.Time `json:"usedAt,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Persistence",type=boolean,JSONPath=`.spec.persistence.allowed`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && self.metadata.name.size() <= 63",message="Tenant name is the client certificate CN: a DNS-1123 label of at most 63 characters"

// Tenant is the Schema for the tenants API: a client of the management agent with
// its own objects, persistence policy and resource quota. The chart always creates
// the tenant "default" and the tenants listed in its values.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantList contains a list of Tenant.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
