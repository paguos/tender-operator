package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IdentitySpec is the desired state of an Identity.
// +kubebuilder:validation:XValidation:rule="has(self.credentialsSecret) == has(oldSelf.credentialsSecret) && (!has(self.credentialsSecret) || self.credentialsSecret == oldSelf.credentialsSecret)",message="credentialsSecret is immutable"
type IdentitySpec struct {
	// CredentialsSecret is the name of the Secret, in this namespace, that
	// receives the service account's key. Defaults to "<name>-gcp". Immutable.
	// +kubebuilder:validation:MinLength=1
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// IdentityStatus is the observed state of an Identity.
type IdentityStatus struct {
	// ServiceAccount is the email of the Google service account.
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// ObservedGeneration is the generation the status was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Identity is a Google service account for an app, with its key delivered as
// a Secret. Other resources (such as Buckets) grant access to it.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service Account",type=string,JSONPath=`.status.serviceAccount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Identity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IdentitySpec   `json:"spec,omitempty"`
	Status IdentityStatus `json:"status,omitempty"`
}

// IdentityList is a list of Identities.
// +kubebuilder:object:root=true
type IdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Identity `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Identity{}, &IdentityList{})
}

// SecretName returns the name of the credentials Secret.
func (i *Identity) SecretName() string {
	if i.Spec.CredentialsSecret != "" {
		return i.Spec.CredentialsSecret
	}
	return i.Name + "-gcp"
}
