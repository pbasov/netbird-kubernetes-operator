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

package v1

import (
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NBNodePeerSpec defines the desired state of NBNodePeer.
type NBNodePeerSpec struct {
	// NodeSelector defines which nodes should have setup keys created.
	// +kubebuilder:validation:Required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// SetupKeyType specifies the type of setup key to create.
	// +kubebuilder:validation:Enum=one-off;reusable
	// +kubebuilder:default=one-off
	// +optional
	SetupKeyType string `json:"setupKeyType,omitempty"`

	// Ephemeral when true, the peer will be auto-removed when offline for more than 10 minutes.
	// +kubebuilder:default=true
	// +optional
	Ephemeral *bool `json:"ephemeral,omitempty"`

	// AutoGroups specifies the NetBird groups to assign the peer to.
	// +kubebuilder:validation:MinItems=1
	AutoGroups []string `json:"autoGroups"`

	// ExpiresIn specifies the setup key expiration in seconds. 0 means never expires.
	// +optional
	ExpiresIn *int `json:"expiresIn,omitempty"`

	// UsageLimit specifies how many times the key can be used. 0 means unlimited.
	// Only relevant for reusable keys.
	// +optional
	UsageLimit *int `json:"usageLimit,omitempty"`

	// SecretNamespace defines where to create the secrets containing setup keys.
	// +kubebuilder:default=netbird-system
	// +optional
	SecretNamespace string `json:"secretNamespace,omitempty"`

	// SecretNameTemplate defines the naming pattern for secrets.
	// Available template variable: {{.NodeName}}
	// +kubebuilder:default="nb-node-{{.NodeName}}"
	// +optional
	SecretNameTemplate string `json:"secretNameTemplate,omitempty"`

	// RevokeOnDelete when true, revokes the setup key in NetBird when the node is deleted.
	// +kubebuilder:default=true
	// +optional
	RevokeOnDelete *bool `json:"revokeOnDelete,omitempty"`
}

// NBNodePeerNodeStatus tracks the status for a single node.
type NBNodePeerNodeStatus struct {
	// SetupKeyID is the ID of the setup key in NetBird.
	SetupKeyID string `json:"setupKeyID"`

	// SecretName is the name of the secret containing the setup key.
	SecretName string `json:"secretName"`

	// LastSyncTime is when this node was last synchronized.
	LastSyncTime metav1.Time `json:"lastSyncTime"`
}

// NBNodePeerStatus defines the observed state of NBNodePeer.
type NBNodePeerStatus struct {
	// Nodes tracks the status of each managed node.
	// +optional
	Nodes map[string]NBNodePeerNodeStatus `json:"nodes,omitempty"`

	// Conditions represent the current state of the NBNodePeer.
	// +optional
	Conditions []NBCondition `json:"conditions,omitempty"`
}

// Equal returns if NBNodePeerStatus is equal to this one.
func (a NBNodePeerStatus) Equal(b NBNodePeerStatus) bool {
	if len(a.Nodes) != len(b.Nodes) {
		return false
	}
	for k, v := range a.Nodes {
		bv, ok := b.Nodes[k]
		if !ok {
			return false
		}
		if v.SetupKeyID != bv.SetupKeyID || v.SecretName != bv.SecretName {
			return false
		}
	}
	return reflect.DeepEqual(a.Conditions, b.Conditions)
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// NBNodePeer is the Schema for the nbnodepeers API.
type NBNodePeer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NBNodePeerSpec   `json:"spec,omitempty"`
	Status NBNodePeerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NBNodePeerList contains a list of NBNodePeer.
type NBNodePeerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NBNodePeer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NBNodePeer{}, &NBNodePeerList{})
}
