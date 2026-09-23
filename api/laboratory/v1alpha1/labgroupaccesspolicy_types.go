package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LabGroupAccessAction is applied to every matching client/lab pair. Deny
// rules always take precedence over allow rules; a pair matching no allow rule
// is denied by default.
// +kubebuilder:validation:Enum=allow;deny
type LabGroupAccessAction string

const (
	LabGroupAccessAllow LabGroupAccessAction = "allow"
	LabGroupAccessDeny  LabGroupAccessAction = "deny"
)

// LabGroupAccessRule targets selected clients and laboratories in one group.
// An empty ClientNames list means every LabGroupClient in this namespace.
// An empty LabNames list means every Lab in this namespace.
type LabGroupAccessRule struct {
	Action LabGroupAccessAction `json:"action"`
	// +optional
	ClientNames []string `json:"clientNames,omitempty"`
	// +optional
	LabNames []string `json:"labNames,omitempty"`
}

// LabGroupAccessPolicySpec is a complete replacement desired policy. Empty is
// intentional and means no VPN client can reach a laboratory.
type LabGroupAccessPolicySpec struct {
	// +optional
	Rules []LabGroupAccessRule `json:"rules,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=lgap

// LabGroupAccessPolicy is the namespaced firewall policy of one LabGroup. The
// agent owns a fixed object in the LabGroup namespace and replaces its Spec in
// one Kubernetes update, while the VPN process watches this resource.
type LabGroupAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LabGroupAccessPolicySpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

type LabGroupAccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabGroupAccessPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LabGroupAccessPolicy{}, &LabGroupAccessPolicyList{})
}
