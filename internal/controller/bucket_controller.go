// Package controller holds the reconcilers, one per kind.
package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
	"github.com/paguos/tender-operator/internal/cloud"
	"github.com/paguos/tender-operator/internal/naming"
)

const (
	// Finalizer keeps a Bucket until its cloud resources are cleaned up.
	Finalizer = "cloud.paguos.dev/finalizer"
	// KeyIDAnnotation on the credentials Secret names the key it holds.
	KeyIDAnnotation = "cloud.paguos.dev/key-id"
	// ManagedByLabel marks objects created by tender. The manager only caches
	// Secrets with this label.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "tender"

	// BucketRole is granted to the bucket's service account.
	BucketRole = "roles/storage.objectAdmin"

	ConditionReady = "Ready"
)

// BucketReconciler reconciles Bucket objects.
type BucketReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Cloud   cloud.Provider
	Project string
	Prefix  string        // prepended to generated cloud names
	Resync  time.Duration // how often a Ready bucket is checked for drift
	Recheck time.Duration // how often a blocked deletion is retried
}

// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=buckets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=buckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=buckets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b cloudv1alpha1.Bucket
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !b.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &b)
	}

	if controllerutil.AddFinalizer(&b, Finalizer) {
		if err := r.Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.reconcile(ctx, &b); err != nil {
		r.setReady(&b, metav1.ConditionFalse, reason(err), err.Error())
		if uerr := r.Status().Update(ctx, &b); uerr != nil {
			log.FromContext(ctx).Error(uerr, "update status")
		}
		return ctrl.Result{}, err
	}

	r.setReady(&b, metav1.ConditionTrue, "Provisioned", "bucket and credentials are ready")
	b.Status.ObservedGeneration = b.Generation
	if err := r.Status().Update(ctx, &b); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.Resync}, nil
}

// reconcileError carries a condition reason.
type reconcileError struct {
	reason string
	err    error
}

func (e *reconcileError) Error() string { return e.err.Error() }
func (e *reconcileError) Unwrap() error { return e.err }

func fail(reason string, format string, args ...any) error {
	return &reconcileError{reason: reason, err: fmt.Errorf(format, args...)}
}

func reason(err error) string {
	var re *reconcileError
	if errors.As(err, &re) {
		return re.reason
	}
	return "Error"
}

func (r *BucketReconciler) reconcile(ctx context.Context, b *cloudv1alpha1.Bucket) error {
	name := r.bucketName(b)
	b.Status.BucketName = name
	b.Status.URL = "gs://" + name

	spec := cloud.BucketSpec{
		Name:         name,
		Location:     b.Spec.Location,
		StorageClass: b.Spec.StorageClass,
		Labels: map[string]string{
			"managed-by": ManagedByValue,
			"namespace":  naming.LabelValue(b.Namespace),
			"name":       naming.LabelValue(b.Name),
		},
	}
	cur, err := r.Cloud.GetBucket(ctx, name)
	switch {
	case errors.Is(err, cloud.ErrNotFound):
		if err := r.Cloud.CreateBucket(ctx, spec); err != nil {
			return fail("BucketFailed", "create bucket %s: %w", name, err)
		}
	case err != nil:
		return fail("BucketFailed", "get bucket %s: %w", name, err)
	default:
		if cur.Labels["managed-by"] != ManagedByValue {
			return fail("BucketConflict", "bucket %s exists and is not managed by tender", name)
		}
		if cur.Labels["namespace"] != spec.Labels["namespace"] || cur.Labels["name"] != spec.Labels["name"] {
			return fail("BucketConflict", "bucket %s belongs to %s/%s", name, cur.Labels["namespace"], cur.Labels["name"])
		}
		if !strings.EqualFold(cur.Location, spec.Location) {
			return fail("LocationMismatch", "bucket %s is in %s, spec says %s (location cannot change)", name, cur.Location, spec.Location)
		}
		if cur.StorageClass != spec.StorageClass || !maps.Equal(cur.Labels, spec.Labels) {
			if err := r.Cloud.UpdateBucket(ctx, spec); err != nil {
				return fail("BucketFailed", "update bucket %s: %w", name, err)
			}
		}
	}

	email, err := r.Cloud.EnsureServiceAccount(ctx, r.accountID(b), fmt.Sprintf("tender: %s/%s", b.Namespace, b.Name))
	if err != nil {
		return fail("ServiceAccountFailed", "service account: %w", err)
	}
	b.Status.ServiceAccount = email

	// A new service account can take a few seconds to be usable in IAM policies;
	// the error requeues with backoff.
	if err := r.Cloud.GrantBucketRole(ctx, name, BucketRole, "serviceAccount:"+email); err != nil {
		return fail("IAMFailed", "grant %s on %s: %w", BucketRole, name, err)
	}

	return r.reconcileSecret(ctx, b, name, email)
}

func (r *BucketReconciler) reconcileSecret(ctx context.Context, b *cloudv1alpha1.Bucket, bucket, email string) error {
	keys, err := r.Cloud.ListKeys(ctx, email)
	if err != nil {
		return fail("KeyFailed", "list keys: %w", err)
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: b.Namespace, Name: b.SecretName()}
	err = r.Get(ctx, key, secret)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return fail("SecretFailed", "get secret %s: %w", key.Name, err)
	}
	if exists && !metav1.IsControlledBy(secret, b) {
		return fail("SecretConflict", "secret %s exists and is not owned by this Bucket", key.Name)
	}

	current := secret.Annotations[KeyIDAnnotation]
	var keyJSON []byte
	if !exists || !slices.Contains(keys, current) || len(secret.Data["credentials.json"]) == 0 {
		k, err := r.Cloud.CreateKey(ctx, email)
		if err != nil {
			return fail("KeyFailed", "create key: %w", err)
		}
		current, keyJSON = k.ID, k.JSON
	} else {
		keyJSON = secret.Data["credentials.json"]
	}

	secret.ObjectMeta = metav1.ObjectMeta{
		Name:            key.Name,
		Namespace:       key.Namespace,
		ResourceVersion: secret.ResourceVersion,
		Labels:          map[string]string{ManagedByLabel: ManagedByValue},
		Annotations:     map[string]string{KeyIDAnnotation: current},
	}
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = map[string][]byte{
		"credentials.json": keyJSON,
		"BUCKET_NAME":      []byte(bucket),
		"GCP_PROJECT":      []byte(r.Project),
	}
	if err := controllerutil.SetControllerReference(b, secret, r.Scheme); err != nil {
		return err
	}
	if exists {
		err = r.Update(ctx, secret)
	} else {
		err = r.Create(ctx, secret)
	}
	if err != nil {
		return fail("SecretFailed", "write secret %s: %w", key.Name, err)
	}

	// Only now that the Secret holds the current key, drop the others
	// (rotated, or created by a reconcile that failed to write the Secret).
	for _, id := range keys {
		if id == current {
			continue
		}
		if err := r.Cloud.DeleteKey(ctx, email, id); err != nil && !errors.Is(err, cloud.ErrNotFound) {
			return fail("KeyFailed", "delete old key %s: %w", id, err)
		}
	}
	return nil
}

func (r *BucketReconciler) finalize(ctx context.Context, b *cloudv1alpha1.Bucket) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, Finalizer) {
		return ctrl.Result{}, nil
	}

	// The credentials Secret is owned by the Bucket and garbage-collected.
	// The service account only exists for this Bucket, so it goes either way.
	email := b.Status.ServiceAccount
	if email == "" {
		email = fmt.Sprintf("%s@%s.iam.gserviceaccount.com", r.accountID(b), r.Project)
	}
	if err := r.Cloud.DeleteServiceAccount(ctx, email); err != nil && !errors.Is(err, cloud.ErrNotFound) {
		return ctrl.Result{}, fmt.Errorf("delete service account: %w", err)
	}

	if b.Spec.DeletionPolicy == cloudv1alpha1.DeletionPolicyDelete {
		name := r.bucketName(b)
		owned, err := r.ownsBucket(ctx, b, name)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("check bucket: %w", err)
		}
		empty := false
		if owned {
			empty, err = r.Cloud.BucketEmpty(ctx, name)
		}
		switch {
		case !owned, errors.Is(err, cloud.ErrNotFound):
			// Gone already, or someone else's bucket: never delete it.
		case err != nil:
			return ctrl.Result{}, fmt.Errorf("check bucket: %w", err)
		case !empty:
			r.setReady(b, metav1.ConditionFalse, "DeletionBlocked", fmt.Sprintf("bucket %s is not empty; empty it to finish deleting", name))
			if err := r.Status().Update(ctx, b); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: r.Recheck}, nil
		default:
			if err := r.Cloud.DeleteBucket(ctx, name); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return ctrl.Result{}, fmt.Errorf("delete bucket: %w", err)
			}
		}
	}

	controllerutil.RemoveFinalizer(b, Finalizer)
	return ctrl.Result{}, r.Update(ctx, b)
}

// ownsBucket reports whether the bucket exists and carries this Bucket's labels.
func (r *BucketReconciler) ownsBucket(ctx context.Context, b *cloudv1alpha1.Bucket, name string) (bool, error) {
	cur, err := r.Cloud.GetBucket(ctx, name)
	if errors.Is(err, cloud.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cur.Labels["managed-by"] == ManagedByValue &&
		cur.Labels["namespace"] == naming.LabelValue(b.Namespace) &&
		cur.Labels["name"] == naming.LabelValue(b.Name), nil
}

func (r *BucketReconciler) bucketName(b *cloudv1alpha1.Bucket) string {
	if b.Spec.BucketName != "" {
		return b.Spec.BucketName
	}
	return naming.BucketName(r.Prefix, b.Namespace, b.Name)
}

func (r *BucketReconciler) accountID(b *cloudv1alpha1.Bucket) string {
	return naming.AccountID(r.Prefix, b.Namespace, b.Name)
}

func (r *BucketReconciler) setReady(b *cloudv1alpha1.Bucket, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: b.Generation,
	})
}

// SetupWithManager registers the controller.
func (r *BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Resync == 0 {
		r.Resync = 10 * time.Minute
	}
	if r.Recheck == 0 {
		r.Recheck = time.Minute
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.Bucket{}).
		Owns(&corev1.Secret{}).
		Named("bucket").
		Complete(r)
}
