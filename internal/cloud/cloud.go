// Package cloud is the boundary between the controllers and the cloud provider.
// Controllers only talk to Provider, so tests can use the in-memory fake
// (package cloud/fake) instead of Google Cloud.
package cloud

import (
	"context"
	"errors"
)

// ErrNotFound is returned (possibly wrapped) when a resource does not exist.
var ErrNotFound = errors.New("not found")

// BucketSpec is the desired state of a bucket.
type BucketSpec struct {
	Name         string
	Location     string
	StorageClass string
	Labels       map[string]string
}

// Bucket is the observed state of a bucket.
type Bucket struct {
	Name         string
	Location     string
	StorageClass string
	Labels       map[string]string
}

// Key is a service-account key.
type Key struct {
	ID   string
	JSON []byte // only set when the key is created
}

// Provider manages buckets, service accounts and their keys.
// Every method must be idempotent where that makes sense.
type Provider interface {
	// GetBucket returns ErrNotFound if the bucket does not exist.
	GetBucket(ctx context.Context, name string) (*Bucket, error)
	CreateBucket(ctx context.Context, spec BucketSpec) error
	// UpdateBucket sets the mutable fields (storage class, labels).
	UpdateBucket(ctx context.Context, spec BucketSpec) error
	// BucketEmpty reports whether the bucket holds no objects.
	BucketEmpty(ctx context.Context, name string) (bool, error)
	// DeleteBucket deletes an empty bucket. ErrNotFound if it does not exist.
	DeleteBucket(ctx context.Context, name string) error
	// GrantBucketRole adds member to role on the bucket's IAM policy.
	GrantBucketRole(ctx context.Context, bucket, role, member string) error

	// EnsureServiceAccount creates the account if needed and returns its email.
	EnsureServiceAccount(ctx context.Context, accountID, displayName string) (string, error)
	// DeleteServiceAccount deletes the account and its keys. ErrNotFound if it does not exist.
	DeleteServiceAccount(ctx context.Context, email string) error
	// ListKeys lists the IDs of the account's user-managed keys.
	ListKeys(ctx context.Context, email string) ([]string, error)
	CreateKey(ctx context.Context, email string) (*Key, error)
	// DeleteKey returns ErrNotFound if the key does not exist.
	DeleteKey(ctx context.Context, email, keyID string) error
}
