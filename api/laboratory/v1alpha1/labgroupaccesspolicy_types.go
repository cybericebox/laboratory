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

// LabGroupAccessRule targets selected clients and VPN gateways (connectors) of selected laboratories in one group. A VPN gateway is a flat
// segment: its own /24 and nothing behind it. Access is per client and per connector, never per whole laboratory: an allow rule names the
// laboratories AND the gateways, and may narrow what of the gateway's /24 is reachable with Include.
//
// An empty ClientNames list means every LabGroupClient in this namespace. An allow rule needs LabNames; it reaches over VPN only the gateways in GatewayNames (empty GatewayNames: no VPN reach, only the browser access
// of the laboratory, which the proxy decides per laboratory); a deny rule with an
// empty LabNames or GatewayNames list means every laboratory or every VPN gateway. Deny always wins over allow.
// +kubebuilder:validation:XValidation:rule="self.action != 'allow' || (has(self.labNames) && size(self.labNames) > 0)",message="an allow rule names the laboratories"
// +kubebuilder:validation:XValidation:rule="!has(self.include) || size(self.include) == 0 || (self.action == 'allow' && has(self.gatewayNames) && size(self.gatewayNames) > 0)",message="include needs an allow rule that names gateways"
type LabGroupAccessRule struct {
	Action LabGroupAccessAction `json:"action"`
	// +optional
	// +kubebuilder:validation:MaxItems=256
	ClientNames []string `json:"clientNames,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=256
	LabNames []string `json:"labNames,omitempty"`
	// GatewayNames are the VPN gateways (connectors) of those laboratories the rule is about, by the name in the lab's spec.gateways.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	GatewayNames []string `json:"gatewayNames,omitempty"`
	// Include narrows the access to these subnets of each named gateway's /24 (the union of the entries). Empty means the whole /24. There are
	// no ranges and no excludes. An entry is relative to the gateway, because its /24 is allocated by the operator: it always lies inside it.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Include []AccessInclude `json:"include,omitempty"`
}

// AccessInclude is a subnet of a gateway's /24: <the /24>.Host/Prefix. Prefix is 24 (the whole /24, Host 0) to 32 (one address), and Host is
// the first address of the subnet inside the /24, so it is a multiple of the subnet size (a /26 starts at 0, 64, 128 or 192).
// +kubebuilder:validation:XValidation:rule="self.host % (self.prefix == 32 ? 1 : self.prefix == 31 ? 2 : self.prefix == 30 ? 4 : self.prefix == 29 ? 8 : self.prefix == 28 ? 16 : self.prefix == 27 ? 32 : self.prefix == 26 ? 64 : self.prefix == 25 ? 128 : 256) == 0",message="host is not the first address of a subnet of that size"
type AccessInclude struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	Host int32 `json:"host"`
	// +kubebuilder:validation:Minimum=24
	// +kubebuilder:validation:Maximum=32
	Prefix int32 `json:"prefix"`
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
	ClientName string               `json:"clientName"`
	LabName    string               `json:"labName"`
	Action     LabGroupAccessAction `json:"action"`
	// GatewayNames are the connectors of the laboratory the relation covers (the counters are those of the client in the laboratory, over all of
	// them).
	GatewayNames []string `json:"gatewayNames,omitempty"`
	Packets      int64    `json:"packets,omitempty"`
	Bytes        int64    `json:"bytes,omitempty"`
	CounterReset bool     `json:"counterReset,omitempty"`
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
