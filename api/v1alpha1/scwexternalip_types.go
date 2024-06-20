/*
Copyright 2023.

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

// ScwNodeExternalIP defines the status of one attached IP
type ScwNodeExternalIP struct {
	// IP represents an attached routed IP
	IP string `json:"ip,omitempty"`
	// IPID represents an attached routed IP's ID
	IPID string `json:"ipID,omitempty"`
	// Zone is the zone in which the IP exists
	Zone string `json:"zone,omitempty"`
	// Node is the name of the node on which the IP is attached
	Node string `json:"node,omitempty"`
	// NodeID is the scw ID of the node on which the IP is attached
	NodeID string `json:"nodeID,omitempty"`
	//NodeMac is the MAC address of the virtual interface
	NodeMacAddr string `json:"nodeMacAddr,omitempty"`
	//PrivateNetworkId is the ID of the PN where the IP is attached to
	PrivateNetworkId *string `json:"privateNetworkId,omitempty"`
}

// ScwExternalIPStatusPendingIP defines the status of one pending IP
type ScwExternalIPStatusPendingIP struct {
	// IP represents an attached routed IP
	IP string `json:"ip,omitempty"`
	// Zone is the zone in which the IP exists
	Zone string `json:"zone,omitempty"`
	// Reason is the reason why the IP is not attached
	Reason string `json:"reason,omitempty"`
}

// ScwExternalIPSpec defines the desired state of ScwExternalIP
type ScwExternalIPSpec struct {
	// Important: Run "make" to regenerate code after modifying this file

	// Service is the name of the targeted Kubernetes service, which contains the scaleway routed IPs
	// +kubebuilder:validation:MinLength=1
	Service string `json:"service,omitempty"`

	// NodeSelector selects on which nodes the IPs will be attached
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Private Network is the id of the private network where the IP have to be mounted
	PrivateNetwork string `json:"privateNetwork,omitempty"`
}

// ScwExternalIPStatus defines the observed state of ScwExternalIP
type ScwExternalIPStatus struct {
	AttachedIPs []ScwNodeExternalIP            `json:"attachedIPs,omitempty"`
	DeletingIPs []ScwNodeExternalIP            `json:"deletingIPs,omitempty"`
	PendingIPs  []ScwExternalIPStatusPendingIP `json:"pendingIPs,omitempty"`
	// Are these really useful though, juste to have some sugar around kubectl
	IPs             []string `json:"ips,omitempty"`
	PendingIPsCount uint     `json:"pendingIPsCount,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.service`
// +kubebuilder:printcolumn:name="Pending IPs Count",type=string,JSONPath=`.status.pendingIPsCount`
// +kubebuilder:printcolumn:name="Attached IPs",type=string,JSONPath=`.status.ips`

// ScwExternalIP is the Schema for the scwexternalips API
type ScwExternalIP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScwExternalIPSpec   `json:"spec,omitempty"`
	Status ScwExternalIPStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ScwExternalIPList contains a list of ScwExternalIP
type ScwExternalIPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScwExternalIP `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScwExternalIP{}, &ScwExternalIPList{})
}
