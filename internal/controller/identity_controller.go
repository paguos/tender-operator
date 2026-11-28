package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// IdentityReconciler reconciles Identity objects: a Google service account,
// a Secret with one of its keys, and the project roles it asks for.
type IdentityReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Cloud   cloud.Provider
	Project string
	Prefix  string        // prepended to generated service-account IDs
	Resync  time.Duration // how often a Ready identity is checked for drift
	// AllowedProjectRoles are the only roles spec.projectRoles may name.
	// Empty means no project roles are granted.
	AllowedProjectRoles []string
}

// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=identities,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=identities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.paguos.dev,resources=identities/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *IdentityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var id cloudv1alpha1.Identity
	if err := r.Get(ctx, req.NamespacedName, &id); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !id.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &id)
	}

	if controllerutil.AddFinalizer(&id, Finalizer) {
		if err := r.Update(ctx, &id); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Status is written as a merge patch against this snapshot: it doesn't
	// carry a resourceVersion, so it can't conflict with other writers.
	base := id.DeepCopy()

	if err := r.reconcile(ctx, &id); err != nil {
		setReady(&id.Status.Conditions, id.Generation, metav1.ConditionFalse, reason(err), err.Error())
		if uerr := r.Status().Patch(ctx, &id, client.MergeFrom(base)); uerr != nil {
			log.FromContext(ctx).Error(uerr, "update status")
		}
		return ctrl.Result{}, err
	}

	setReady(&id.Status.Conditions, id.Generation, metav1.ConditionTrue, "Provisioned", "service account and credentials are ready")
	id.Status.ObservedGeneration = id.Generation
	if err := r.Status().Patch(ctx, &id, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.Resync}, nil
}

func (r *IdentityReconciler) reconcile(ctx context.Context, id *cloudv1alpha1.Identity) error {
	email, err := r.Cloud.EnsureServiceAccount(ctx, r.accountID(id), fmt.Sprintf("tender: %s/%s", id.Namespace, id.Name))
	if err != nil {
		return fail("ServiceAccountFailed", "service account: %w", err)
	}
	id.Status.ServiceAccount = email
	if err := r.reconcileSecret(ctx, id, email); err != nil {
		return err
	}
	return r.reconcileProjectRoles(ctx, id, email)
}

// reconcileProjectRoles grants spec.projectRoles and revokes roles tender
// granted earlier that are no longer wanted. status.projectRoles tracks what
// tender granted, and stays accurate even when a call fails midway. Nothing
// changes while spec names a role that isn't allowed.
func (r *IdentityReconciler) reconcileProjectRoles(ctx context.Context, id *cloudv1alpha1.Identity, email string) error {
	var denied []string
	for _, role := range id.Spec.ProjectRoles {
		if !slices.Contains(r.AllowedProjectRoles, role) {
			denied = append(denied, role)
		}
	}
	if len(denied) > 0 {
		return fail("RoleNotAllowed", "project roles not allowed by the operator: %s", strings.Join(denied, ", "))
	}

	member := "serviceAccount:" + email
	for _, role := range id.Spec.ProjectRoles {
		if err := r.Cloud.GrantProjectRole(ctx, role, member); err != nil {
			// A new service account can take a few seconds to be usable in IAM
			// policies; the error requeues with backoff.
			return fail("IAMFailed", "grant %s: %w", role, err)
		}
		if !slices.Contains(id.Status.ProjectRoles, role) {
			id.Status.ProjectRoles = append(id.Status.ProjectRoles, role)
		}
	}
	for _, role := range slices.Clone(id.Status.ProjectRoles) {
		if slices.Contains(id.Spec.ProjectRoles, role) {
			continue
		}
		if err := r.Cloud.RevokeProjectRole(ctx, role, member); err != nil {
			return fail("IAMFailed", "revoke %s: %w", role, err)
		}
		id.Status.ProjectRoles = slices.DeleteFunc(id.Status.ProjectRoles, func(x string) bool { return x == role })
	}
	slices.Sort(id.Status.ProjectRoles)
	return nil
}

func (r *IdentityReconciler) reconcileSecret(ctx context.Context, id *cloudv1alpha1.Identity, email string) error {
	keys, err := r.Cloud.ListKeys(ctx, email)
	if err != nil {
		return fail("KeyFailed", "list keys: %w", err)
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: id.Namespace, Name: id.SecretName()}
	err = r.Get(ctx, key, secret)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return fail("SecretFailed", "get secret %s: %w", key.Name, err)
	}
	if exists && !metav1.IsControlledBy(secret, id) {
		return fail("SecretConflict", "secret %s exists and is not owned by this Identity", key.Name)
	}

	current := secret.Annotations[KeyIDAnnotation]
	keyJSON := secret.Data["credentials.json"]
	if !exists || !slices.Contains(keys, current) || len(keyJSON) == 0 {
		k, err := r.Cloud.CreateKey(ctx, email)
		if err != nil {
			return fail("KeyFailed", "create key: %w", err)
		}
		current, keyJSON = k.ID, k.JSON
	}

	secret.ObjectMeta = metav1.ObjectMeta{
		Name:            key.Name,
		Namespace:       key.Namespace,
		ResourceVersion: secret.ResourceVersion,
		Labels:          managedLabels(),
		Annotations:     map[string]string{KeyIDAnnotation: current},
	}
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = map[string][]byte{
		"credentials.json":      keyJSON,
		"GCP_PROJECT":           []byte(r.Project),
		"SERVICE_ACCOUNT_EMAIL": []byte(email),
	}
	if err := controllerutil.SetControllerReference(id, secret, r.Scheme); err != nil {
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
	for _, k := range keys {
		if k == current {
			continue
		}
		if err := r.Cloud.DeleteKey(ctx, email, k); err != nil && !errors.Is(err, cloud.ErrNotFound) {
			return fail("KeyFailed", "delete old key %s: %w", k, err)
		}
	}
	return nil
}

func (r *IdentityReconciler) finalize(ctx context.Context, id *cloudv1alpha1.Identity) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(id, Finalizer) {
		return ctrl.Result{}, nil
	}
	// Project bindings would otherwise linger as "deleted:serviceAccount:...".
	email := r.email(id)
	for _, role := range id.Status.ProjectRoles {
		if err := r.Cloud.RevokeProjectRole(ctx, role, "serviceAccount:"+email); err != nil {
			return ctrl.Result{}, fmt.Errorf("revoke %s: %w", role, err)
		}
	}
	// The Secret is owned by the Identity and garbage-collected. Deleting the
	// service account also deletes its keys; bindings to it on buckets stop
	// working at once and are cleaned up by the Buckets.
	if err := r.Cloud.DeleteServiceAccount(ctx, email); err != nil && !errors.Is(err, cloud.ErrNotFound) {
		return ctrl.Result{}, fmt.Errorf("delete service account: %w", err)
	}
	controllerutil.RemoveFinalizer(id, Finalizer)
	return ctrl.Result{}, r.Update(ctx, id)
}

func (r *IdentityReconciler) accountID(id *cloudv1alpha1.Identity) string {
	return naming.AccountID(r.Prefix, id.Namespace, id.Name)
}

func (r *IdentityReconciler) email(id *cloudv1alpha1.Identity) string {
	if id.Status.ServiceAccount != "" {
		return id.Status.ServiceAccount
	}
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", r.accountID(id), r.Project)
}

// SetupWithManager registers the controller.
func (r *IdentityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Resync == 0 {
		r.Resync = 10 * time.Minute
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.Identity{}).
		Owns(&corev1.Secret{}).
		Named("identity").
		Complete(r)
}
