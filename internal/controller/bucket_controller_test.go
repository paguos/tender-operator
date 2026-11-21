package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
	"github.com/paguos/tender-operator/internal/cloud"
	"github.com/paguos/tender-operator/internal/cloud/fake"
)

const (
	project    = "test-project"
	ns         = "shop-dev"
	wantBucket = "tender-shop-dev-files"
)

type env struct {
	t     *testing.T
	ctx   context.Context
	cloud *fake.Provider
	kube  client.Client
	r     *BucketReconciler
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&cloudv1alpha1.Bucket{}).
		WithObjects(objs...).
		Build()
	fc := fake.New(project)
	return &env{
		t:     t,
		ctx:   context.Background(),
		cloud: fc,
		kube:  kube,
		r: &BucketReconciler{
			Client:  kube,
			Scheme:  scheme,
			Cloud:   fc,
			Project: project,
			Prefix:  "tender",
			Resync:  10,
			Recheck: 5,
		},
	}
}

func newBucket(mut ...func(*cloudv1alpha1.Bucket)) *cloudv1alpha1.Bucket {
	b := &cloudv1alpha1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "files", Namespace: ns, UID: types.UID("uid-1")},
		Spec: cloudv1alpha1.BucketSpec{
			Location:       "EU",
			StorageClass:   "STANDARD",
			DeletionPolicy: cloudv1alpha1.DeletionPolicyRetain,
		},
	}
	for _, m := range mut {
		m(b)
	}
	return b
}

func (e *env) reconcile() (ctrl.Result, error) {
	e.t.Helper()
	return e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "files"}})
}

func (e *env) mustReconcile() ctrl.Result {
	e.t.Helper()
	res, err := e.reconcile()
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (e *env) bucket() *cloudv1alpha1.Bucket {
	e.t.Helper()
	var b cloudv1alpha1.Bucket
	if err := e.kube.Get(e.ctx, types.NamespacedName{Namespace: ns, Name: "files"}, &b); err != nil {
		e.t.Fatalf("get bucket: %v", err)
	}
	return &b
}

func (e *env) secret() *corev1.Secret {
	e.t.Helper()
	var s corev1.Secret
	if err := e.kube.Get(e.ctx, types.NamespacedName{Namespace: ns, Name: "files-gcs"}, &s); err != nil {
		e.t.Fatalf("get secret: %v", err)
	}
	return &s
}

func ready(b *cloudv1alpha1.Bucket) *metav1.Condition {
	return meta.FindStatusCondition(b.Status.Conditions, ConditionReady)
}

func email(e *env) string {
	return e.bucket().Status.ServiceAccount
}

func TestProvisions(t *testing.T) {
	e := newEnv(t, newBucket())
	res := e.mustReconcile()
	if res.RequeueAfter != e.r.Resync {
		t.Errorf("RequeueAfter = %v, want resync %v", res.RequeueAfter, e.r.Resync)
	}

	b := e.bucket()
	if c := ready(b); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v", c)
	}
	if b.Status.BucketName != wantBucket || b.Status.URL != "gs://"+wantBucket {
		t.Errorf("status = %+v", b.Status)
	}
	if len(b.Finalizers) != 1 || b.Finalizers[0] != Finalizer {
		t.Errorf("finalizers = %v", b.Finalizers)
	}

	got, err := e.cloud.GetBucket(e.ctx, wantBucket)
	if err != nil {
		t.Fatal(err)
	}
	if got.Location != "EU" || got.Labels["managed-by"] != "tender" || got.Labels["namespace"] != ns || got.Labels["name"] != "files" {
		t.Errorf("cloud bucket = %+v", got)
	}

	sa := b.Status.ServiceAccount
	if !e.cloud.HasAccount(sa) {
		t.Fatalf("service account %q missing", sa)
	}
	if m := e.cloud.Members(wantBucket, BucketRole); len(m) != 1 || m[0] != "serviceAccount:"+sa {
		t.Errorf("members of %s = %v", BucketRole, m)
	}

	s := e.secret()
	keys := e.cloud.Keys(sa)
	if len(keys) != 1 || s.Annotations[KeyIDAnnotation] != keys[0] {
		t.Errorf("keys = %v, secret key-id = %q", keys, s.Annotations[KeyIDAnnotation])
	}
	if string(s.Data["BUCKET_NAME"]) != wantBucket || string(s.Data["GCP_PROJECT"]) != project || len(s.Data["credentials.json"]) == 0 {
		t.Errorf("secret data = %v", s.Data)
	}
	if s.Labels[ManagedByLabel] != ManagedByValue {
		t.Errorf("secret labels = %v", s.Labels)
	}
	if !metav1.IsControlledBy(s, b) {
		t.Error("secret is not controlled by the Bucket")
	}
}

func TestIdempotent(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcile()
	before := e.secret().Annotations[KeyIDAnnotation]
	e.mustReconcile()
	e.mustReconcile()

	if after := e.secret().Annotations[KeyIDAnnotation]; after != before {
		t.Errorf("key changed on a no-op reconcile: %s -> %s", before, after)
	}
	if keys := e.cloud.Keys(email(e)); len(keys) != 1 {
		t.Errorf("keys = %v, want one", keys)
	}
}

func TestDeletedSecretGetsNewKey(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcile()
	old := e.secret().Annotations[KeyIDAnnotation]

	if err := e.kube.Delete(e.ctx, e.secret()); err != nil {
		t.Fatal(err)
	}
	e.mustReconcile()

	cur := e.secret().Annotations[KeyIDAnnotation]
	if cur == old {
		t.Fatal("expected a new key")
	}
	if keys := e.cloud.Keys(email(e)); len(keys) != 1 || keys[0] != cur {
		t.Errorf("keys = %v, want only %s", keys, cur)
	}
}

func TestStaleKeysAreDeleted(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcile()
	sa := email(e)
	if _, err := e.cloud.CreateKey(e.ctx, sa); err != nil {
		t.Fatal(err)
	}
	e.mustReconcile()
	if keys := e.cloud.Keys(sa); len(keys) != 1 || keys[0] != e.secret().Annotations[KeyIDAnnotation] {
		t.Errorf("keys = %v", keys)
	}
}

func TestRevokedKeyIsReplaced(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcile()
	sa, old := email(e), e.secret().Annotations[KeyIDAnnotation]
	if err := e.cloud.DeleteKey(e.ctx, sa, old); err != nil {
		t.Fatal(err)
	}
	e.mustReconcile()
	if cur := e.secret().Annotations[KeyIDAnnotation]; cur == old {
		t.Error("secret still holds the revoked key")
	}
}

func TestDriftIsCorrected(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcile()
	if err := e.cloud.UpdateBucket(e.ctx, cloud.BucketSpec{
		Name: wantBucket, StorageClass: "COLDLINE",
		Labels: map[string]string{"managed-by": "tender", "namespace": ns, "name": "files", "extra": "x"},
	}); err != nil {
		t.Fatal(err)
	}
	e.mustReconcile()
	got, _ := e.cloud.GetBucket(e.ctx, wantBucket)
	if got.StorageClass != "STANDARD" || got.Labels["extra"] != "" {
		t.Errorf("drift not corrected: %+v", got)
	}
}

func TestBucketNameOverride(t *testing.T) {
	e := newEnv(t, newBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.BucketName = "my-custom-bucket" }))
	e.mustReconcile()
	if !e.cloud.HasBucket("my-custom-bucket") {
		t.Error("custom bucket not created")
	}
}

func TestForeignBucketConflicts(t *testing.T) {
	e := newEnv(t, newBucket())
	if err := e.cloud.CreateBucket(e.ctx, cloud.BucketSpec{Name: wantBucket, Location: "EU"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcile(); err == nil {
		t.Fatal("expected an error")
	}
	b := e.bucket()
	if c := ready(b); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "BucketConflict" {
		t.Errorf("Ready = %+v", c)
	}
	if b.Status.ServiceAccount != "" {
		t.Error("service account created for a foreign bucket")
	}
}

func TestOtherObjectsBucketConflicts(t *testing.T) {
	e := newEnv(t, newBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.BucketName = "shared" }))
	if err := e.cloud.CreateBucket(e.ctx, cloud.BucketSpec{Name: "shared", Location: "EU",
		Labels: map[string]string{"managed-by": "tender", "namespace": "other", "name": "files"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcile(); err == nil {
		t.Fatal("expected an error")
	}
	if c := ready(e.bucket()); c.Reason != "BucketConflict" {
		t.Errorf("reason = %s", c.Reason)
	}
}

func TestLocationMismatch(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcile()
	b := e.bucket()
	b.Spec.Location = "US"
	if err := e.kube.Update(e.ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcile(); err == nil {
		t.Fatal("expected an error")
	}
	if c := ready(e.bucket()); c.Reason != "LocationMismatch" {
		t.Errorf("reason = %s", c.Reason)
	}
}

func TestForeignSecretConflicts(t *testing.T) {
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "files-gcs", Namespace: ns}}
	e := newEnv(t, newBucket(), foreign)
	if _, err := e.reconcile(); err == nil {
		t.Fatal("expected an error")
	}
	if c := ready(e.bucket()); c.Reason != "SecretConflict" {
		t.Errorf("reason = %s", c.Reason)
	}
	if keys := e.cloud.Keys(email(e)); len(keys) != 0 {
		t.Errorf("created keys %v despite the conflict", keys)
	}
}

func TestCloudErrorSetsNotReady(t *testing.T) {
	e := newEnv(t, newBucket())
	e.cloud.Err = errors.New("boom")
	if _, err := e.reconcile(); err == nil {
		t.Fatal("expected an error")
	}
	c := ready(e.bucket())
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "BucketFailed" {
		t.Errorf("Ready = %+v", c)
	}

	e.cloud.Err = nil
	e.mustReconcile()
	if c := ready(e.bucket()); c.Status != metav1.ConditionTrue {
		t.Errorf("did not recover: %+v", c)
	}
}

func (e *env) delete() {
	e.t.Helper()
	if err := e.kube.Delete(e.ctx, e.bucket()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) gone() bool {
	var b cloudv1alpha1.Bucket
	err := e.kube.Get(e.ctx, types.NamespacedName{Namespace: ns, Name: "files"}, &b)
	return apierrors.IsNotFound(err)
}

func TestDeleteRetainKeepsBucket(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcile()
	sa := email(e)
	e.cloud.AddObjects(wantBucket, 3)

	e.delete()
	e.mustReconcile()

	if !e.gone() {
		t.Error("Bucket object still exists")
	}
	if !e.cloud.HasBucket(wantBucket) {
		t.Error("Retain deleted the cloud bucket")
	}
	if e.cloud.HasAccount(sa) {
		t.Error("service account not deleted")
	}
}

func TestDeletePolicyWaitsUntilEmpty(t *testing.T) {
	e := newEnv(t, newBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.DeletionPolicy = cloudv1alpha1.DeletionPolicyDelete }))
	e.mustReconcile()
	e.cloud.AddObjects(wantBucket, 1)

	e.delete()
	res := e.mustReconcile()
	if res.RequeueAfter != e.r.Recheck {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, e.r.Recheck)
	}
	if e.gone() || !e.cloud.HasBucket(wantBucket) {
		t.Fatal("deleted a bucket that is not empty")
	}
	if c := ready(e.bucket()); c.Reason != "DeletionBlocked" {
		t.Errorf("reason = %s", c.Reason)
	}

	e.cloud.AddObjects(wantBucket, -1)
	e.mustReconcile()
	if !e.gone() || e.cloud.HasBucket(wantBucket) {
		t.Error("empty bucket not deleted")
	}
}

func TestDeletePolicyNeverDeletesForeignBucket(t *testing.T) {
	e := newEnv(t, newBucket(func(b *cloudv1alpha1.Bucket) {
		b.Spec.BucketName = "theirs"
		b.Spec.DeletionPolicy = cloudv1alpha1.DeletionPolicyDelete
	}))
	if err := e.cloud.CreateBucket(e.ctx, cloud.BucketSpec{Name: "theirs", Location: "EU"}); err != nil {
		t.Fatal(err)
	}
	_, _ = e.reconcile() // conflict, but the finalizer is on

	e.delete()
	e.mustReconcile()
	if !e.gone() {
		t.Error("Bucket object still exists")
	}
	if !e.cloud.HasBucket("theirs") {
		t.Error("deleted a bucket tender does not own")
	}
}

func TestDeleteWithoutCloudResources(t *testing.T) {
	e := newEnv(t, newBucket(func(b *cloudv1alpha1.Bucket) {
		b.Finalizers = []string{Finalizer}
		b.Spec.DeletionPolicy = cloudv1alpha1.DeletionPolicyDelete
	}))
	e.delete()
	e.mustReconcile()
	if !e.gone() {
		t.Error("Bucket object still exists")
	}
}
