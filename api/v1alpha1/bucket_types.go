package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeletionPolicy says what happens to the cloud bucket when the Bucket object is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain keeps the bucket and its objects.
	DeletionPolicyRetain DeletionPolicy = "Retain"
	// DeletionPolicyDelete deletes the bucket, but only once it is empty.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// BucketSpec is the desired state of a Bucket.
// +kubebuilder:validation:XValidation:rule="has(self.bucketName) == has(oldSelf.bucketName) && (!has(self.bucketName) || self.bucketName == oldSelf.bucketName)",message="bucketName is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.credentialsSecret) == has(oldSelf.credentialsSecret) && (!has(self.credentialsSecret) || self.credentialsSecret == oldSelf.credentialsSecret)",message="credentialsSecret is immutable"
type BucketSpec struct {
	// Location of the bucket, e.g. EU, US or europe-west3. Immutable.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="location is immutable"
	Location string `json:"location"`

	// StorageClass of the bucket.
	// +kubebuilder:validation:Enum=STANDARD;NEARLINE;COLDLINE;ARCHIVE
	// +kubebuilder:default=STANDARD
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// BucketName overrides the generated bucket name. Bucket names are global,
	// so the generated name includes the operator's name prefix, the namespace
	// and the object name. Immutable.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9_.-]*[a-z0-9]$`
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// DeletionPolicy says what happens to the bucket when this object is deleted.
	// Delete only removes an empty bucket; a bucket with objects blocks deletion
	// until it is emptied.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// CredentialsSecret is the name of the Secret, in this namespace, that
	// receives a service-account key with object admin access to this bucket.
	// Defaults to "<name>-gcs". Immutable.
	// +kubebuilder:validation:MinLength=1
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// BucketStatus is the observed state of a Bucket.
type BucketStatus struct {
	// BucketName is the name of the cloud bucket.
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// URL of the bucket (gs://...).
	// +optional
	URL string `json:"url,omitempty"`

	// ServiceAccount is the email of the service account that can use the bucket.
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

// Bucket is a Google Cloud Storage bucket plus credentials for using it.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.status.bucketName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BucketSpec   `json:"spec,omitempty"`
	Status BucketStatus `json:"status,omitempty"`
}

// BucketList is a list of Buckets.
// +kubebuilder:object:root=true
type BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Bucket `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Bucket{}, &BucketList{})
}

// SecretName returns the name of the credentials Secret.
func (b *Bucket) SecretName() string {
	if b.Spec.CredentialsSecret != "" {
		return b.Spec.CredentialsSecret
	}
	return b.Name + "-gcs"
}
