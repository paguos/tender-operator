package controller

import (
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
	"github.com/paguos/tender-operator/internal/cloud"
)

const wantBucket = "tender-shop-dev-files"

func newBucket(mut ...func(*cloudv1alpha1.Bucket)) *cloudv1alpha1.Bucket {
	b := &cloudv1alpha1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "files", Namespace: ns, UID: types.UID("uid-files")},
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

func withAccess(refs ...string) func(*cloudv1alpha1.Bucket) {
	return func(b *cloudv1alpha1.Bucket) {
		for _, r := range refs {
			b.Spec.Access = append(b.Spec.Access, cloudv1alpha1.BucketAccess{IdentityRef: r})
		}
	}
}

func withPolicy(p cloudv1alpha1.DeletionPolicy) func(*cloudv1alpha1.Bucket) {
	return func(b *cloudv1alpha1.Bucket) { b.Spec.DeletionPolicy = p }
}

func (e *env) reconcileBucket() (ctrl.Result, error) {
	e.t.Helper()
	return e.bucket.Reconcile(e.ctx, req("files"))
}

func (e *env) mustReconcileBucket() ctrl.Result {
	e.t.Helper()
	res, err := e.reconcileBucket()
	if err != nil {
		e.t.Fatalf("reconcile bucket: %v", err)
	}
	return res
}

func (e *env) getBucket() *cloudv1alpha1.Bucket {
	e.t.Helper()
	var b cloudv1alpha1.Bucket
	e.get("files", &b)
	return &b
}

func (e *env) updateBucket(mut func(*cloudv1alpha1.Bucket)) {
	e.t.Helper()
	b := e.getBucket()
	mut(b)
	if err := e.kube.Update(e.ctx, b); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) bucketReason() string {
	e.t.Helper()
	c := readyOf(e.getBucket().Status.Conditions)
	if c == nil {
		return ""
	}
	return c.Reason
}

// member returns the IAM member of a reconciled Identity.
func (e *env) member(name string) string {
	e.t.Helper()
	return "serviceAccount:" + e.getIdentity(name).Status.ServiceAccount
}

func TestBucketProvisions(t *testing.T) {
	e := newEnv(t, newBucket())
	if res := e.mustReconcileBucket(); res.RequeueAfter != resync {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, resync)
	}

	b := e.getBucket()
	if c := readyOf(b.Status.Conditions); c == nil || c.Status != metav1.ConditionTrue {
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
	if len(e.cloud.Bindings(wantBucket)) != 0 {
		t.Errorf("bindings without access: %v", e.cloud.Bindings(wantBucket))
	}

	var cm corev1.ConfigMap
	e.get("files-bucket", &cm)
	if cm.Data["BUCKET_NAME"] != wantBucket || cm.Data["BUCKET_URL"] != "gs://"+wantBucket || cm.Data["GCP_PROJECT"] != project {
		t.Errorf("configmap data = %v", cm.Data)
	}
	if !metav1.IsControlledBy(&cm, b) || cm.Labels[ManagedByLabel] != ManagedByValue {
		t.Errorf("configmap not owned or labelled: %+v", cm.ObjectMeta)
	}
}

func TestBucketGrantsAccess(t *testing.T) {
	e := newEnv(t, newIdentity("app"), newIdentity("reader"), newBucket(func(b *cloudv1alpha1.Bucket) {
		b.Spec.Access = []cloudv1alpha1.BucketAccess{
			{IdentityRef: "app"}, // role defaults to objectAdmin
			{IdentityRef: "reader", Role: cloudv1alpha1.BucketRoleObjectViewer},
		}
	}))
	e.mustReconcileIdentity("app")
	e.mustReconcileIdentity("reader")
	e.mustReconcileBucket()

	if m := e.cloud.Members(wantBucket, "roles/storage.objectAdmin"); !slices.Equal(m, []string{e.member("app")}) {
		t.Errorf("objectAdmin = %v", m)
	}
	if m := e.cloud.Members(wantBucket, "roles/storage.objectViewer"); !slices.Equal(m, []string{e.member("reader")}) {
		t.Errorf("objectViewer = %v", m)
	}
	if n := len(e.getBucket().Status.Bindings); n != 2 {
		t.Errorf("status.bindings = %v", e.getBucket().Status.Bindings)
	}
}

func TestBucketWaitsForIdentity(t *testing.T) {
	e := newEnv(t, newBucket(withAccess("app")))

	res := e.mustReconcileBucket() // no error: waiting is not a failure
	if res.RequeueAfter != recheck || e.bucketReason() != "IdentityNotFound" {
		t.Fatalf("RequeueAfter = %v, reason = %s", res.RequeueAfter, e.bucketReason())
	}
	if !e.cloud.HasBucket(wantBucket) {
		t.Error("bucket should be created while waiting for the identity")
	}

	if err := e.kube.Create(e.ctx, newIdentity("app")); err != nil {
		t.Fatal(err)
	}
	e.mustReconcileBucket()
	if e.bucketReason() != "IdentityNotReady" {
		t.Fatalf("reason = %s", e.bucketReason())
	}

	e.mustReconcileIdentity("app")
	e.mustReconcileBucket()
	if c := readyOf(e.getBucket().Status.Conditions); c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v", c)
	}
	if m := e.cloud.Members(wantBucket, "roles/storage.objectAdmin"); !slices.Equal(m, []string{e.member("app")}) {
		t.Errorf("objectAdmin = %v", m)
	}
}

func TestBucketRevokesRemovedAccess(t *testing.T) {
	e := newEnv(t, newIdentity("app"), newBucket(withAccess("app")))
	e.mustReconcileIdentity("app")
	e.mustReconcileBucket()

	e.updateBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.Access = nil })
	e.mustReconcileBucket()

	if bs := e.cloud.Bindings(wantBucket); len(bs) != 0 {
		t.Errorf("bindings left: %v", bs)
	}
	if bs := e.getBucket().Status.Bindings; len(bs) != 0 {
		t.Errorf("status.bindings = %v", bs)
	}
}

func TestBucketRoleChange(t *testing.T) {
	e := newEnv(t, newIdentity("app"), newBucket(withAccess("app")))
	e.mustReconcileIdentity("app")
	e.mustReconcileBucket()

	e.updateBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.Access[0].Role = cloudv1alpha1.BucketRoleObjectViewer })
	e.mustReconcileBucket()

	bs := e.cloud.Bindings(wantBucket)
	if len(bs) != 1 || !slices.Equal(bs["roles/storage.objectViewer"], []string{e.member("app")}) {
		t.Errorf("bindings = %v", bs)
	}
}

func TestBucketLeavesOtherBindingsAlone(t *testing.T) {
	e := newEnv(t, newIdentity("app"), newBucket(withAccess("app")))
	e.mustReconcileIdentity("app")
	e.mustReconcileBucket()
	if err := e.cloud.GrantBucketRole(e.ctx, wantBucket, "roles/storage.objectViewer", "user:someone@example.com"); err != nil {
		t.Fatal(err)
	}

	e.updateBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.Access = nil })
	e.mustReconcileBucket()

	if m := e.cloud.Members(wantBucket, "roles/storage.objectViewer"); !slices.Equal(m, []string{"user:someone@example.com"}) {
		t.Errorf("removed a binding tender did not grant: %v", m)
	}
}

func TestBucketRestoresDriftedBinding(t *testing.T) {
	e := newEnv(t, newIdentity("app"), newBucket(withAccess("app")))
	e.mustReconcileIdentity("app")
	e.mustReconcileBucket()
	if err := e.cloud.RevokeBucketRole(e.ctx, wantBucket, "roles/storage.objectAdmin", e.member("app")); err != nil {
		t.Fatal(err)
	}
	e.mustReconcileBucket()
	if m := e.cloud.Members(wantBucket, "roles/storage.objectAdmin"); len(m) != 1 {
		t.Errorf("binding not restored: %v", m)
	}
}

func TestBucketDriftIsCorrected(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcileBucket()
	if err := e.cloud.UpdateBucket(e.ctx, cloud.BucketSpec{
		Name: wantBucket, StorageClass: "COLDLINE",
		Labels: map[string]string{"managed-by": "tender", "namespace": ns, "name": "files", "extra": "x"},
	}); err != nil {
		t.Fatal(err)
	}
	e.mustReconcileBucket()
	got, _ := e.cloud.GetBucket(e.ctx, wantBucket)
	if got.StorageClass != "STANDARD" || got.Labels["extra"] != "" {
		t.Errorf("drift not corrected: %+v", got)
	}
}

func TestBucketNameOverride(t *testing.T) {
	e := newEnv(t, newBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.BucketName = "my-custom-bucket" }))
	e.mustReconcileBucket()
	if !e.cloud.HasBucket("my-custom-bucket") {
		t.Error("custom bucket not created")
	}
}

func TestBucketForeignBucketConflicts(t *testing.T) {
	e := newEnv(t, newIdentity("app"), newBucket(withAccess("app")))
	e.mustReconcileIdentity("app")
	if err := e.cloud.CreateBucket(e.ctx, cloud.BucketSpec{Name: wantBucket, Location: "EU"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcileBucket(); err == nil {
		t.Fatal("expected an error")
	}
	if e.bucketReason() != "BucketConflict" {
		t.Errorf("reason = %s", e.bucketReason())
	}
	if bs := e.cloud.Bindings(wantBucket); len(bs) != 0 {
		t.Errorf("granted access on a foreign bucket: %v", bs)
	}
}

func TestBucketOtherObjectsBucketConflicts(t *testing.T) {
	e := newEnv(t, newBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.BucketName = "shared" }))
	if err := e.cloud.CreateBucket(e.ctx, cloud.BucketSpec{Name: "shared", Location: "EU",
		Labels: map[string]string{"managed-by": "tender", "namespace": "other", "name": "files"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcileBucket(); err == nil {
		t.Fatal("expected an error")
	}
	if e.bucketReason() != "BucketConflict" {
		t.Errorf("reason = %s", e.bucketReason())
	}
}

func TestBucketLocationMismatch(t *testing.T) {
	e := newEnv(t, newBucket())
	e.mustReconcileBucket()
	e.updateBucket(func(b *cloudv1alpha1.Bucket) { b.Spec.Location = "US" })
	if _, err := e.reconcileBucket(); err == nil {
		t.Fatal("expected an error")
	}
	if e.bucketReason() != "LocationMismatch" {
		t.Errorf("reason = %s", e.bucketReason())
	}
}

func TestBucketForeignConfigMapConflicts(t *testing.T) {
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "files-bucket", Namespace: ns}}
	e := newEnv(t, newBucket(), foreign)
	if _, err := e.reconcileBucket(); err == nil {
		t.Fatal("expected an error")
	}
	if e.bucketReason() != "ConfigMapConflict" {
		t.Errorf("reason = %s", e.bucketReason())
	}
}

func TestBucketCloudErrorRecovers(t *testing.T) {
	e := newEnv(t, newBucket())
	e.cloud.Err = errors.New("boom")
	if _, err := e.reconcileBucket(); err == nil {
		t.Fatal("expected an error")
	}
	if e.bucketReason() != "BucketFailed" {
		t.Errorf("reason = %s", e.bucketReason())
	}
	e.cloud.Err = nil
	e.mustReconcileBucket()
	if c := readyOf(e.getBucket().Status.Conditions); c.Status != metav1.ConditionTrue {
		t.Errorf("did not recover: %+v", c)
	}
}

func TestBucketDeleteRetainKeepsBucketAndRevokes(t *testing.T) {
	e := newEnv(t, newIdentity("app"), newBucket(withAccess("app")))
	e.mustReconcileIdentity("app")
	e.mustReconcileBucket()
	e.cloud.AddObjects(wantBucket, 3)

	e.delete("files", &cloudv1alpha1.Bucket{})
	e.mustReconcileBucket()

	if !e.gone("files", &cloudv1alpha1.Bucket{}) {
		t.Error("Bucket object still exists")
	}
	if !e.cloud.HasBucket(wantBucket) {
		t.Error("Retain deleted the cloud bucket")
	}
	if bs := e.cloud.Bindings(wantBucket); len(bs) != 0 {
		t.Errorf("bindings left on a retained bucket: %v", bs)
	}
	if !e.cloud.HasAccount(e.getIdentity("app").Status.ServiceAccount) {
		t.Error("deleting a Bucket must not delete the Identity's service account")
	}
}

func TestBucketDeletePolicyWaitsUntilEmpty(t *testing.T) {
	e := newEnv(t, newBucket(withPolicy(cloudv1alpha1.DeletionPolicyDelete)))
	e.mustReconcileBucket()
	e.cloud.AddObjects(wantBucket, 1)

	e.delete("files", &cloudv1alpha1.Bucket{})
	if res := e.mustReconcileBucket(); res.RequeueAfter != recheck {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, recheck)
	}
	if e.gone("files", &cloudv1alpha1.Bucket{}) || !e.cloud.HasBucket(wantBucket) {
		t.Fatal("deleted a bucket that is not empty")
	}
	if e.bucketReason() != "DeletionBlocked" {
		t.Errorf("reason = %s", e.bucketReason())
	}

	e.cloud.AddObjects(wantBucket, -1)
	e.mustReconcileBucket()
	if !e.gone("files", &cloudv1alpha1.Bucket{}) || e.cloud.HasBucket(wantBucket) {
		t.Error("empty bucket not deleted")
	}
}

func TestBucketDeleteNeverTouchesForeignBucket(t *testing.T) {
	e := newEnv(t, newBucket(withPolicy(cloudv1alpha1.DeletionPolicyDelete), func(b *cloudv1alpha1.Bucket) {
		b.Spec.BucketName = "theirs"
	}))
	if err := e.cloud.CreateBucket(e.ctx, cloud.BucketSpec{Name: "theirs", Location: "EU"}); err != nil {
		t.Fatal(err)
	}
	_, _ = e.reconcileBucket() // conflict, but the finalizer is on

	e.delete("files", &cloudv1alpha1.Bucket{})
	e.mustReconcileBucket()
	if !e.gone("files", &cloudv1alpha1.Bucket{}) {
		t.Error("Bucket object still exists")
	}
	if !e.cloud.HasBucket("theirs") {
		t.Error("deleted a bucket tender does not own")
	}
}

func TestBucketDeleteBeforeProvisioning(t *testing.T) {
	e := newEnv(t, newBucket(withPolicy(cloudv1alpha1.DeletionPolicyDelete), func(b *cloudv1alpha1.Bucket) {
		b.Finalizers = []string{Finalizer}
	}))
	e.delete("files", &cloudv1alpha1.Bucket{})
	e.mustReconcileBucket()
	if !e.gone("files", &cloudv1alpha1.Bucket{}) {
		t.Error("Bucket object still exists")
	}
}

func TestBucketsForIdentity(t *testing.T) {
	other := newBucket(func(b *cloudv1alpha1.Bucket) {
		b.Name, b.UID = "other", "uid-other"
		b.Spec.Access = []cloudv1alpha1.BucketAccess{{IdentityRef: "someone-else"}}
	})
	elsewhere := newBucket(withAccess("app"), func(b *cloudv1alpha1.Bucket) {
		b.Namespace, b.UID = "elsewhere", "uid-elsewhere"
	})
	e := newEnv(t, newBucket(withAccess("app")), other, elsewhere)

	got := e.bucket.bucketsForIdentity(e.ctx, newIdentity("app"))
	want := []reconcile.Request{req("files")}
	if !slices.Equal(got, want) {
		t.Errorf("bucketsForIdentity = %v, want %v", got, want)
	}
}
