/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Mode string

const (
	// ModeHub indicates that the unmanaged switch operates in hub mode.
	ModeHub Mode = "hub"
	// ModeSwitch indicates that the unmanaged switch operates in switch mode.
	ModeSwitch Mode = "switch"
)

// UnmanagedSwitchSpec defines the desired state of UnmanagedSwitch.
type UnmanagedSwitchSpec struct {
	// Laboratory is the name of the laboratory to which this unmanaged switch belongs.
	// +kubebuilder:validation:Required
	Laboratory string `json:"laboratory,omitempty"`
	// Mode indicates the operational mode of the unmanaged switch.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=hub;switch
	Mode Mode `json:"mode,omitempty"`
	// Ports define the ports of the unmanaged switch.
	// +kubebuilder:validation:Required
	Ports []DevicePort `json:"ports,omitempty"`
}

// UnmanagedSwitchStatus defines the observed state of UnmanagedSwitch.
type UnmanagedSwitchStatus struct {
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// UnmanagedSwitch is the Schema for the unmanagedswitches API.
type UnmanagedSwitch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UnmanagedSwitchSpec   `json:"spec,omitempty"`
	Status UnmanagedSwitchStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UnmanagedSwitchList contains a list of UnmanagedSwitch.
type UnmanagedSwitchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UnmanagedSwitch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UnmanagedSwitch{}, &UnmanagedSwitchList{})
}
