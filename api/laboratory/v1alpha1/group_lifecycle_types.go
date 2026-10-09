package v1alpha1

// GroupLifecycleSpec stops only group services after every child is released.
// A start affects VPN/gateway only, never a child's lifecycle or terminal intent.
// +kubebuilder:validation:XValidation:rule="self.desiredState != 'Stopped' || self.requireAllLabsStopped",message="group stop requires all labs stopped"
// +kubebuilder:validation:XValidation:rule="self.revision > oldSelf.revision || self == oldSelf",message="group lifecycle revision must increase for changed intent"
type GroupLifecycleSpec struct {
	// +kubebuilder:validation:Enum=Running;Stopped
	DesiredState string `json:"desiredState"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	OperationID string `json:"operationId"`
	// +kubebuilder:validation:Minimum=1
	Revision              int64 `json:"revision"`
	RequireAllLabsStopped bool  `json:"requireAllLabsStopped"`
}

func (s *GroupLifecycleSpec) IsStopped() bool { return s != nil && s.DesiredState == "Stopped" }

// GroupChildAdmission is a durable pending write, never a completion certificate.
// No timeout cancels it: identical retries resume after an agent restart.
type GroupChildAdmission struct {
	Token          string `json:"token"`
	GroupUID       string `json:"groupUID"`
	LabName        string `json:"labName"`
	ExpectedLabUID string `json:"expectedLabUID,omitempty"`
	OperationID    string `json:"operationId,omitempty"`
	Revision       int64  `json:"revision,omitempty"`
	DesiredState   string `json:"desiredState"`
	SpecHash       string `json:"specHash,omitempty"`
}

// LabCreationReceipt records original birth separately from lifecycle/native
// observations. Pending records cannot be rebound to a recreated name.
type LabCreationReceipt struct {
	LabName        string `json:"labName"`
	GroupUID       string `json:"groupUID"`
	NamespaceUID   string `json:"namespaceUID"`
	OperationID    string `json:"operationId"`
	Revision       int64  `json:"revision"`
	DefinitionHash string `json:"definitionHash"`
	CreationID     string `json:"creationId"`
	LabUID         string `json:"labUID,omitempty"`
	Committed      bool   `json:"committed"`
}
