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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
	"github.com/paguos/tender-operator/internal/cloud"
	"github.com/paguos/tender-operator/internal/naming"
)

// BucketReconciler reconciles Bucket objects.
type BucketReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Cloud   cloud.Provider
	Project string
	Prefix  string        // prepended to generated bucket names
	Resync  time.Duration // how often a Ready bucket is checked for drift
	Recheck time.Duration // how often a blocked deletion or missing identity is retried
}

// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=buckets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=buckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=buckets/finalizers,verbs=update
// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=identities,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch

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

	// Status is written as a merge patch against this snapshot: it doesn't
	// carry a resourceVersion, so it can't conflict with other writers.
	base := b.DeepCopy()

	if err := r.reconcile(ctx, &b); err != nil {
		setReady(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, reason(err), err.Error())
		if uerr := r.Status().Patch(ctx, &b, client.MergeFrom(base)); uerr != nil {
			log.FromContext(ctx).Error(uerr, "update status")
		}
		// A missing or unready Identity is not a failure of ours: wait for it
		// (the Identity watch requeues as soon as it changes) without backoff.
		if why := reason(err); why == "IdentityNotFound" || why == "IdentityNotReady" {
			return ctrl.Result{RequeueAfter: r.Recheck}, nil
		}
		return ctrl.Result{}, err
	}

	setReady(&b.Status.Conditions, b.Generation, metav1.ConditionTrue, "Provisioned", "bucket and access are ready")
	b.Status.ObservedGeneration = b.Generation
	if err := r.Status().Patch(ctx, &b, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.Resync}, nil
}

func (r *BucketReconciler) reconcile(ctx context.Context, b *cloudv1alpha1.Bucket) error {
	name := r.bucketName(b)
	b.Status.BucketName = name
	b.Status.URL = "gs://" + name

	if err := r.reconcileBucket(ctx, b, name); err != nil {
		return err
	}
	if err := r.reconcileConfigMap(ctx, b, name); err != nil {
		return err
	}
	return r.reconcileAccess(ctx, b, name)
}

func (r *BucketReconciler) labels(b *cloudv1alpha1.Bucket) map[string]string {
	return map[string]string{
		"managed-by": ManagedByValue,
		"namespace":  naming.LabelValue(b.Namespace),
		"name":       naming.LabelValue(b.Name),
	}
}

func (r *BucketReconciler) reconcileBucket(ctx context.Context, b *cloudv1alpha1.Bucket, name string) error {
	spec := cloud.BucketSpec{
		Name:         name,
		Location:     b.Spec.Location,
		StorageClass: b.Spec.StorageClass,
		Labels:       r.labels(b),
	}
	cur, err := r.Cloud.GetBucket(ctx, name)
	switch {
	case errors.Is(err, cloud.ErrNotFound):
		if err := r.Cloud.CreateBucket(ctx, spec); err != nil {
			return fail("BucketFailed", "create bucket %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fail("BucketFailed", "get bucket %s: %w", name, err)
	}
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
	return nil
}

// reconcileConfigMap publishes the bucket's name for apps to read.
func (r *BucketReconciler) reconcileConfigMap(ctx context.Context, b *cloudv1alpha1.Bucket, name string) error {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: b.Namespace, Name: b.ConfigMapName()}
	err := r.Get(ctx, key, cm)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return fail("ConfigMapFailed", "get configmap %s: %w", key.Name, err)
	}
	if exists && !metav1.IsControlledBy(cm, b) {
		return fail("ConfigMapConflict", "configmap %s exists and is not owned by this Bucket", key.Name)
	}
	data := map[string]string{
		"BUCKET_NAME": name,
		"BUCKET_URL":  "gs://" + name,
		"GCP_PROJECT": r.Project,
	}
	if exists && maps.Equal(cm.Data, data) && maps.Equal(cm.Labels, managedLabels()) {
		return nil
	}
	cm.ObjectMeta = metav1.ObjectMeta{
		Name:            key.Name,
		Namespace:       key.Namespace,
		ResourceVersion: cm.ResourceVersion,
		Labels:          managedLabels(),
	}
	cm.Data = data
	if err := controllerutil.SetControllerReference(b, cm, r.Scheme); err != nil {
		return err
	}
	if exists {
		err = r.Update(ctx, cm)
	} else {
		err = r.Create(ctx, cm)
	}
	if err != nil {
		return fail("ConfigMapFailed", "write configmap %s: %w", key.Name, err)
	}
	return nil
}

// reconcileAccess grants each listed Identity its role and revokes bindings
// tender granted earlier that are no longer wanted. status.bindings tracks
// what tender granted, and stays accurate even when a call fails midway.
func (r *BucketReconciler) reconcileAccess(ctx context.Context, b *cloudv1alpha1.Bucket, name string) error {
	desired, err := r.desiredBindings(ctx, b)
	if err != nil {
		return err
	}

	for _, d := range desired {
		if err := r.Cloud.GrantBucketRole(ctx, name, d.Role, d.Member); err != nil {
			// A new service account can take a few seconds to be usable in IAM
			// policies; the error requeues with backoff.
			return fail("IAMFailed", "grant %s to %s: %w", d.Role, d.Member, err)
		}
		if !slices.Contains(b.Status.Bindings, d) {
			b.Status.Bindings = append(b.Status.Bindings, d)
		}
	}

	for _, have := range slices.Clone(b.Status.Bindings) {
		if slices.Contains(desired, have) {
			continue
		}
		if err := r.Cloud.RevokeBucketRole(ctx, name, have.Role, have.Member); err != nil {
			return fail("IAMFailed", "revoke %s from %s: %w", have.Role, have.Member, err)
		}
		b.Status.Bindings = slices.DeleteFunc(b.Status.Bindings, func(x cloudv1alpha1.BucketBinding) bool { return x == have })
	}
	sortBindings(b.Status.Bindings)
	return nil
}

// desiredBindings resolves spec.access. Nothing is granted or revoked unless
// every referenced Identity is ready.
func (r *BucketReconciler) desiredBindings(ctx context.Context, b *cloudv1alpha1.Bucket) ([]cloudv1alpha1.BucketBinding, error) {
	var out []cloudv1alpha1.BucketBinding
	for _, a := range b.Spec.Access {
		var id cloudv1alpha1.Identity
		err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: a.IdentityRef}, &id)
		if apierrors.IsNotFound(err) {
			return nil, fail("IdentityNotFound", "identity %s not found", a.IdentityRef)
		}
		if err != nil {
			return nil, fail("Error", "get identity %s: %w", a.IdentityRef, err)
		}
		if !id.DeletionTimestamp.IsZero() {
			return nil, fail("IdentityNotFound", "identity %s is being deleted", a.IdentityRef)
		}
		if id.Status.ServiceAccount == "" || !meta.IsStatusConditionTrue(id.Status.Conditions, ConditionReady) {
			return nil, fail("IdentityNotReady", "identity %s is not ready", a.IdentityRef)
		}
		role := a.Role
		if role == "" {
			role = cloudv1alpha1.BucketRoleObjectAdmin
		}
		out = append(out, cloudv1alpha1.BucketBinding{Role: role.IAMRole(), Member: "serviceAccount:" + id.Status.ServiceAccount})
	}
	return out, nil
}

func sortBindings(bs []cloudv1alpha1.BucketBinding) {
	slices.SortFunc(bs, func(a, b cloudv1alpha1.BucketBinding) int {
		if c := strings.Compare(a.Role, b.Role); c != 0 {
			return c
		}
		return strings.Compare(a.Member, b.Member)
	})
}

func (r *BucketReconciler) finalize(ctx context.Context, b *cloudv1alpha1.Bucket) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, Finalizer) {
		return ctrl.Result{}, nil
	}
	// The ConfigMap is owned by the Bucket and garbage-collected.
	name := r.bucketName(b)
	owned, err := r.ownsBucket(ctx, b, name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check bucket: %w", err)
	}
	// A bucket that's gone, or someone else's, is never touched.
	if owned {
		done, err := r.cleanUp(ctx, b, name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: r.Recheck}, nil
		}
	}
	controllerutil.RemoveFinalizer(b, Finalizer)
	return ctrl.Result{}, r.Update(ctx, b)
}

// cleanUp deletes the bucket (policy Delete) or revokes tender's bindings on
// it (policy Retain). It returns false while deletion is blocked.
func (r *BucketReconciler) cleanUp(ctx context.Context, b *cloudv1alpha1.Bucket, name string) (bool, error) {
	if b.Spec.DeletionPolicy == cloudv1alpha1.DeletionPolicyDelete {
		empty, err := r.Cloud.BucketEmpty(ctx, name)
		if errors.Is(err, cloud.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("check bucket: %w", err)
		}
		if !empty {
			base := b.DeepCopy()
			setReady(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, "DeletionBlocked",
				fmt.Sprintf("bucket %s is not empty; empty it to finish deleting", name))
			return false, r.Status().Patch(ctx, b, client.MergeFrom(base))
		}
		if err := r.Cloud.DeleteBucket(ctx, name); err != nil && !errors.Is(err, cloud.ErrNotFound) {
			return false, fmt.Errorf("delete bucket: %w", err)
		}
		return true, nil
	}

	// Retain: the bucket stays, but nobody keeps access through tender.
	for _, have := range b.Status.Bindings {
		if err := r.Cloud.RevokeBucketRole(ctx, name, have.Role, have.Member); err != nil && !errors.Is(err, cloud.ErrNotFound) {
			return false, fmt.Errorf("revoke %s from %s: %w", have.Role, have.Member, err)
		}
	}
	return true, nil
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
	want := r.labels(b)
	return cur.Labels["managed-by"] == want["managed-by"] &&
		cur.Labels["namespace"] == want["namespace"] &&
		cur.Labels["name"] == want["name"], nil
}

func (r *BucketReconciler) bucketName(b *cloudv1alpha1.Bucket) string {
	if b.Spec.BucketName != "" {
		return b.Spec.BucketName
	}
	return naming.BucketName(r.Prefix, b.Namespace, b.Name)
}

// bucketsForIdentity maps an Identity to the Buckets in its namespace that
// reference it, so they reconcile when it becomes ready or goes away.
func (r *BucketReconciler) bucketsForIdentity(ctx context.Context, obj client.Object) []reconcile.Request {
	var list cloudv1alpha1.BucketList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list buckets for identity", "identity", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for _, b := range list.Items {
		if slices.ContainsFunc(b.Spec.Access, func(a cloudv1alpha1.BucketAccess) bool { return a.IdentityRef == obj.GetName() }) {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Name}})
		}
	}
	return reqs
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
		Owns(&corev1.ConfigMap{}).
		Watches(&cloudv1alpha1.Identity{}, handler.EnqueueRequestsFromMapFunc(r.bucketsForIdentity)).
		Named("bucket").
		Complete(r)
}
