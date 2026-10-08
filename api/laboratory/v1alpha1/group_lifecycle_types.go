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
