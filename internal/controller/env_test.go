package controller

import (
	"context"
	"testing"
	"time"

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
	"github.com/paguos/tender-operator/internal/cloud/fake"
)

// Tests run the reconcilers against controller-runtime's fake client and the
// in-memory cloud provider, calling Reconcile directly.

const (
	project = "test-project"
	ns      = "shop-dev"
	resync  = 10 * time.Minute
	recheck = time.Minute
)

type env struct {
	t        *testing.T
	ctx      context.Context
	cloud    *fake.Provider
	kube     client.Client
	identity *IdentityReconciler
	bucket   *BucketReconciler
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
		WithStatusSubresource(&cloudv1alpha1.Bucket{}, &cloudv1alpha1.Identity{}).
		WithObjects(objs...).
		Build()
	fc := fake.New(project)
	return &env{
		t:     t,
		ctx:   context.Background(),
		cloud: fc,
		kube:  kube,
		identity: &IdentityReconciler{
			Client: kube, Scheme: scheme, Cloud: fc, Project: project, Prefix: "tender", Resync: resync,
		},
		bucket: &BucketReconciler{
			Client: kube, Scheme: scheme, Cloud: fc, Project: project, Prefix: "tender", Resync: resync, Recheck: recheck,
		},
	}
}

func req(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func (e *env) get(name string, obj client.Object) {
	e.t.Helper()
	if err := e.kube.Get(e.ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		e.t.Fatalf("get %T %s: %v", obj, name, err)
	}
}

func (e *env) gone(name string, obj client.Object) bool {
	err := e.kube.Get(e.ctx, types.NamespacedName{Namespace: ns, Name: name}, obj)
	return apierrors.IsNotFound(err)
}

func (e *env) delete(name string, obj client.Object) {
	e.t.Helper()
	e.get(name, obj)
	if err := e.kube.Delete(e.ctx, obj); err != nil {
		e.t.Fatal(err)
	}
}

func readyOf(conds []metav1.Condition) *metav1.Condition {
	return meta.FindStatusCondition(conds, ConditionReady)
}
