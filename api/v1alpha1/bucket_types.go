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

// BucketRole is a level of access to a bucket's objects.
// +kubebuilder:validation:Enum=objectViewer;objectCreator;objectAdmin
type BucketRole string

const (
	BucketRoleObjectViewer  BucketRole = "objectViewer"
	BucketRoleObjectCreator BucketRole = "objectCreator"
	BucketRoleObjectAdmin   BucketRole = "objectAdmin"
)

// IAMRole returns the Google Cloud role, e.g. roles/storage.objectAdmin.
func (r BucketRole) IAMRole() string { return "roles/storage." + string(r) }

// BucketAccess grants an Identity in the same namespace a role on the bucket.
type BucketAccess struct {
	// IdentityRef is the name of an Identity in this namespace.
	// +kubebuilder:validation:MinLength=1
	IdentityRef string `json:"identityRef"`

	// Role on the bucket's objects.
	// +kubebuilder:default=objectAdmin
	// +optional
	Role BucketRole `json:"role,omitempty"`
}

// BucketSpec is the desired state of a Bucket.
// +kubebuilder:validation:XValidation:rule="has(self.bucketName) == has(oldSelf.bucketName) && (!has(self.bucketName) || self.bucketName == oldSelf.bucketName)",message="bucketName is immutable"
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

	// Access lists the Identities that may use the bucket. Bindings that tender
	// granted earlier and that are no longer listed are removed.
	// +listType=map
	// +listMapKey=identityRef
	// +optional
	Access []BucketAccess `json:"access,omitempty"`
}

// BucketBinding is an IAM binding tender granted on the bucket.
type BucketBinding struct {
	Role   string `json:"role"`
	Member string `json:"member"`
}

// BucketStatus is the observed state of a Bucket.
type BucketStatus struct {
	// BucketName is the name of the cloud bucket.
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// URL of the bucket (gs://...).
	// +optional
	URL string `json:"url,omitempty"`

	// Bindings are the IAM bindings tender granted, so it can remove them
	// when they leave spec.access.
	// +optional
	Bindings []BucketBinding `json:"bindings,omitempty"`

	// ObservedGeneration is the generation the status was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Bucket is a Google Cloud Storage bucket, plus access for Identities.
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

// ConfigMapName returns the name of the ConfigMap with the bucket's details.
func (b *Bucket) ConfigMapName() string { return b.Name + "-bucket" }
