package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaintenanceWindowSpec announces a period in which the cluster operator may do maintenance
// (draining nodes, upgrades, restarts). Nothing in the platform refuses or delays work during a
// window; tenants read it through the management agent and plan around it (the platform's calendar
// gives the agent no capacity in the window, or the Capacity the window leaves).
type MaintenanceWindowSpec struct {
	// From is when the maintenance may start.
	// +kubebuilder:validation:Required
	From metav1.Time `json:"from"`
	// To is when it is over; empty means open-ended (until the object is deleted or edited).
	// +optional
	To *metav1.Time `json:"to,omitempty"`
	// Reason is shown to tenants, e.g. "kernel upgrade of the lab nodes".
	// +kubebuilder:validation:MaxLength=500
	// +optional
	Reason string `json:"reason,omitempty"`
	// Capacity is what the window leaves to the tenants it applies to (cpu and memory): empty means
	// nothing, the usual case of a drain or an upgrade. A resource the window does not name is zero.
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['cpu', 'memory'])",message="capacity names only cpu and memory"
	// +optional
	Capacity corev1.ResourceList `json:"capacity,omitempty"`
	// Tenants lists the tenants the window applies to (Tenant names, i.e. client certificate
	// CNs); empty means all tenants.
	// +kubebuilder:validation:MaxItems=256
	// +optional
	Tenants []string `json:"tenants,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.spec.from`
// +kubebuilder:printcolumn:name="To",type=string,JSONPath=`.spec.to`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:validation:XValidation:rule="!has(self.spec.to) || self.spec.to > self.spec.from",message="spec.to must be after spec.from"

// MaintenanceWindow is the Schema for the maintenancewindows API: a maintenance period
// announced by the cluster operator.
type MaintenanceWindow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MaintenanceWindowSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// MaintenanceWindowList contains a list of MaintenanceWindow.
type MaintenanceWindowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenanceWindow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaintenanceWindow{}, &MaintenanceWindowList{})
}
