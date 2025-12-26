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
	"github.com/netbirdio/kubernetes-operator/internal/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NBSetupKeyGeneratorSpec defines the desired state of NBSetupKeyGenerator.
type NBSetupKeyGeneratorSpec struct {
	// Name is the name of the setup key in NetBird.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

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

	// SecretName is the name of the secret to create with the setup key.
	// If not specified, defaults to the NBSetupKeyGenerator name.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// RevokeOnDelete when true, revokes the setup key in NetBird when the CR is deleted.
	// +kubebuilder:default=true
	// +optional
	RevokeOnDelete *bool `json:"revokeOnDelete,omitempty"`
}

// NBSetupKeyGeneratorStatus defines the observed state of NBSetupKeyGenerator.
type NBSetupKeyGeneratorStatus struct {
	// SetupKeyID is the ID of the setup key in NetBird.
	// +optional
	SetupKeyID string `json:"setupKeyID,omitempty"`

	// SecretName is the name of the secret containing the setup key.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Conditions represent the current state of the NBSetupKeyGenerator.
	// +optional
	Conditions []NBCondition `json:"conditions,omitempty"`
}

// Equal returns if NBSetupKeyGeneratorStatus is equal to this one
func (a NBSetupKeyGeneratorStatus) Equal(b NBSetupKeyGeneratorStatus) bool {
	return a.SetupKeyID == b.SetupKeyID &&
		a.SecretName == b.SecretName &&
		util.Equivalent(a.Conditions, b.Conditions)
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NBSetupKeyGenerator is the Schema for the nbsetupkeygenerators API.
// It creates a NetBird setup key and stores it in a Kubernetes secret.
type NBSetupKeyGenerator struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NBSetupKeyGeneratorSpec   `json:"spec,omitempty"`
	Status NBSetupKeyGeneratorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NBSetupKeyGeneratorList contains a list of NBSetupKeyGenerator.
type NBSetupKeyGeneratorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NBSetupKeyGenerator `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NBSetupKeyGenerator{}, &NBSetupKeyGeneratorList{})
}
