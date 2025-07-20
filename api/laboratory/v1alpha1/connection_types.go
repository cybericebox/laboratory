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

// ConnectionSpec defines the desired state of Connection.
type ConnectionSpec struct {
	Ports []ConnectionPortSpec `json:"ports,omitempty"` // List of ports to be connected
}

// ConnectionPortSpec defines the desired state of a port in a connection.
type ConnectionPortSpec struct {
	// +kubebuilder:validation:Required
	Device string `json:"device,omitempty"` // Device identifier for the port
	// +kubebuilder:validation:Required
	Port string `json:"port,omitempty"` // Unique identifier for the port
}

// ConnectionStatus defines the observed state of Connection.
type ConnectionStatus struct {
	ConnectionID string                 `json:"connectionID,omitempty"`
	Ports        []ConnectionPortStatus `json:"ports,omitempty"` // List of ports in the
}

type ConnectionPortStatus struct {
	Device string `json:"device,omitempty"` // Device identifier for the port
	Port   string `json:"port,omitempty"`
	PortID string `json:"portID,omitempty"` // Unique identifier for the port
	// Name of the port
	Connected   bool   `json:"connected,omitempty"`   // Indicates if the port is connected
	NodeName    string `json:"nodeName,omitempty"`    // Name of the node where the port is located
	NodeAddress string `json:"nodeAddress,omitempty"` // Address of the node where the port is located
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Connection is the Schema for the connections API.
type Connection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectionSpec   `json:"spec,omitempty"`
	Status ConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectionList contains a list of Connection.
type ConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Connection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Connection{}, &ConnectionList{})
}
