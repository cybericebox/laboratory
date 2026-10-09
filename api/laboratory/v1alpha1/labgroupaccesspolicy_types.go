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
	// Empty identity retains the legacy policy replacement contract.
	// +optional
	OperationID string `json:"operationId,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	Revision int64 `json:"revision,omitempty"`
	// +optional
	ExpectedGroupUID string `json:"expectedGroupUID,omitempty"`
	// +optional
	Rules []LabGroupAccessRule `json:"rules,omitempty"`
}

// LabGroupAccessPolicyRuleStatus is one effective client-to-lab firewall
// relation. Counters are cumulative kernel counters; CounterReset marks a
// replacement/restart so downstream summaries never infer negative traffic.
type LabGroupAccessPolicyRuleStatus struct {
	ClientName   string               `json:"clientName"`
	LabName      string               `json:"labName"`
	Action       LabGroupAccessAction `json:"action"`
	Packets      int64                `json:"packets,omitempty"`
	Bytes        int64                `json:"bytes,omitempty"`
	CounterReset bool                 `json:"counterReset,omitempty"`
}

// LabGroupAccessPolicyStatus is written only by the in-namespace VPN
// reconciler after it has applied the matching policy generation.
type LabGroupAccessPolicyStatus struct {
	// The current VPN boot acknowledges the exact policy operation/revision.
	AppliedRevision    int64                            `json:"appliedRevision,omitempty"`
	OperationID        string                           `json:"operationId,omitempty"`
	VPNBootID          string                           `json:"vpnBootId,omitempty"`
	ObservedGeneration int64                            `json:"observedGeneration,omitempty"`
	State              string                           `json:"state,omitempty"`
	AppliedAt          metav1.Time                      `json:"appliedAt,omitempty"`
	LastError          string                           `json:"lastError,omitempty"`
	Rules              []LabGroupAccessPolicyRuleStatus `json:"rules,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=lgap
// +kubebuilder:subresource:status

// LabGroupAccessPolicy is the namespaced firewall policy of one LabGroup. The
// agent owns a fixed object in the LabGroup namespace and replaces its Spec in
// one Kubernetes update, while the VPN process watches this resource.
type LabGroupAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LabGroupAccessPolicySpec   `json:"spec,omitempty"`
	Status            LabGroupAccessPolicyStatus `json:"status,omitempty"`
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
