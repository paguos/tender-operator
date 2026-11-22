package controller

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
)

func newIdentity(name string, mut ...func(*cloudv1alpha1.Identity)) *cloudv1alpha1.Identity {
	id := &cloudv1alpha1.Identity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
	}
	for _, m := range mut {
		m(id)
	}
	return id
}

func (e *env) reconcileIdentity(name string) (ctrl.Result, error) {
	e.t.Helper()
	return e.identity.Reconcile(e.ctx, req(name))
}

func (e *env) mustReconcileIdentity(name string) ctrl.Result {
	e.t.Helper()
	res, err := e.reconcileIdentity(name)
	if err != nil {
		e.t.Fatalf("reconcile identity %s: %v", name, err)
	}
	return res
}

func (e *env) getIdentity(name string) *cloudv1alpha1.Identity {
	e.t.Helper()
	var id cloudv1alpha1.Identity
	e.get(name, &id)
	return &id
}

func (e *env) getSecret(name string) *corev1.Secret {
	e.t.Helper()
	var s corev1.Secret
	e.get(name, &s)
	return &s
}

func TestIdentityProvisions(t *testing.T) {
	e := newEnv(t, newIdentity("app"))
	if res := e.mustReconcileIdentity("app"); res.RequeueAfter != resync {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, resync)
	}

	id := e.getIdentity("app")
	if c := readyOf(id.Status.Conditions); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v", c)
	}
	sa := id.Status.ServiceAccount
	if !e.cloud.HasAccount(sa) {
		t.Fatalf("service account %q missing", sa)
	}
	if len(id.Finalizers) != 1 || id.Finalizers[0] != Finalizer {
		t.Errorf("finalizers = %v", id.Finalizers)
	}

	s := e.getSecret("app-gcp")
	keys := e.cloud.Keys(sa)
	if len(keys) != 1 || s.Annotations[KeyIDAnnotation] != keys[0] {
		t.Errorf("keys = %v, secret key-id = %q", keys, s.Annotations[KeyIDAnnotation])
	}
	if string(s.Data["SERVICE_ACCOUNT_EMAIL"]) != sa || string(s.Data["GCP_PROJECT"]) != project || len(s.Data["credentials.json"]) == 0 {
		t.Errorf("secret data = %v", s.Data)
	}
	if s.Labels[ManagedByLabel] != ManagedByValue || !metav1.IsControlledBy(s, id) {
		t.Errorf("secret not labelled or not owned: %+v", s.ObjectMeta)
	}
}

func TestIdentityCustomSecretName(t *testing.T) {
	e := newEnv(t, newIdentity("app", func(i *cloudv1alpha1.Identity) { i.Spec.CredentialsSecret = "creds" }))
	e.mustReconcileIdentity("app")
	e.getSecret("creds")
}

func TestIdentityIdempotent(t *testing.T) {
	e := newEnv(t, newIdentity("app"))
	e.mustReconcileIdentity("app")
	before := e.getSecret("app-gcp").Annotations[KeyIDAnnotation]
	e.mustReconcileIdentity("app")
	e.mustReconcileIdentity("app")
	if after := e.getSecret("app-gcp").Annotations[KeyIDAnnotation]; after != before {
		t.Errorf("key changed on a no-op reconcile: %s -> %s", before, after)
	}
	if keys := e.cloud.Keys(e.getIdentity("app").Status.ServiceAccount); len(keys) != 1 {
		t.Errorf("keys = %v, want one", keys)
	}
}

func TestIdentityDeletedSecretGetsNewKey(t *testing.T) {
	e := newEnv(t, newIdentity("app"))
	e.mustReconcileIdentity("app")
	old := e.getSecret("app-gcp").Annotations[KeyIDAnnotation]

	e.delete("app-gcp", &corev1.Secret{})
	e.mustReconcileIdentity("app")

	cur := e.getSecret("app-gcp").Annotations[KeyIDAnnotation]
	if cur == old {
		t.Fatal("expected a new key")
	}
	if keys := e.cloud.Keys(e.getIdentity("app").Status.ServiceAccount); len(keys) != 1 || keys[0] != cur {
		t.Errorf("keys = %v, want only %s", keys, cur)
	}
}

func TestIdentityStaleKeysAreDeleted(t *testing.T) {
	e := newEnv(t, newIdentity("app"))
	e.mustReconcileIdentity("app")
	sa := e.getIdentity("app").Status.ServiceAccount
	if _, err := e.cloud.CreateKey(e.ctx, sa); err != nil {
		t.Fatal(err)
	}
	e.mustReconcileIdentity("app")
	if keys := e.cloud.Keys(sa); len(keys) != 1 || keys[0] != e.getSecret("app-gcp").Annotations[KeyIDAnnotation] {
		t.Errorf("keys = %v", keys)
	}
}

func TestIdentityRevokedKeyIsReplaced(t *testing.T) {
	e := newEnv(t, newIdentity("app"))
	e.mustReconcileIdentity("app")
	sa, old := e.getIdentity("app").Status.ServiceAccount, e.getSecret("app-gcp").Annotations[KeyIDAnnotation]
	if err := e.cloud.DeleteKey(e.ctx, sa, old); err != nil {
		t.Fatal(err)
	}
	e.mustReconcileIdentity("app")
	if cur := e.getSecret("app-gcp").Annotations[KeyIDAnnotation]; cur == old {
		t.Error("secret still holds the revoked key")
	}
}

func TestIdentityForeignSecretConflicts(t *testing.T) {
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "app-gcp", Namespace: ns}}
	e := newEnv(t, newIdentity("app"), foreign)
	if _, err := e.reconcileIdentity("app"); err == nil {
		t.Fatal("expected an error")
	}
	id := e.getIdentity("app")
	if c := readyOf(id.Status.Conditions); c.Reason != "SecretConflict" {
		t.Errorf("reason = %s", c.Reason)
	}
	if keys := e.cloud.Keys(id.Status.ServiceAccount); len(keys) != 0 {
		t.Errorf("created keys %v despite the conflict", keys)
	}
}

func TestIdentityCloudErrorRecovers(t *testing.T) {
	e := newEnv(t, newIdentity("app"))
	e.cloud.Err = errors.New("boom")
	if _, err := e.reconcileIdentity("app"); err == nil {
		t.Fatal("expected an error")
	}
	if c := readyOf(e.getIdentity("app").Status.Conditions); c.Status != metav1.ConditionFalse || c.Reason != "ServiceAccountFailed" {
		t.Errorf("Ready = %+v", c)
	}
	e.cloud.Err = nil
	e.mustReconcileIdentity("app")
	if c := readyOf(e.getIdentity("app").Status.Conditions); c.Status != metav1.ConditionTrue {
		t.Errorf("did not recover: %+v", c)
	}
}

func TestIdentityDeleteRemovesServiceAccount(t *testing.T) {
	e := newEnv(t, newIdentity("app"))
	e.mustReconcileIdentity("app")
	sa := e.getIdentity("app").Status.ServiceAccount

	e.delete("app", &cloudv1alpha1.Identity{})
	e.mustReconcileIdentity("app")

	if !e.gone("app", &cloudv1alpha1.Identity{}) {
		t.Error("Identity still exists")
	}
	if e.cloud.HasAccount(sa) {
		t.Error("service account not deleted")
	}
}

func TestIdentityDeleteBeforeProvisioning(t *testing.T) {
	e := newEnv(t, newIdentity("app", func(i *cloudv1alpha1.Identity) { i.Finalizers = []string{Finalizer} }))
	e.delete("app", &cloudv1alpha1.Identity{})
	e.mustReconcileIdentity("app")
	if !e.gone("app", &cloudv1alpha1.Identity{}) {
		t.Error("Identity still exists")
	}
}
