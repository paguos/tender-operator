package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
	"github.com/paguos/tender-operator/internal/cloud"
	"github.com/paguos/tender-operator/internal/cloud/fake"
)

func ready(status metav1.ConditionStatus, reason string) []metav1.Condition {
	return []metav1.Condition{{Type: "Ready", Status: status, Reason: reason}}
}

func TestCollector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := []runtime.Object{
		&cloudv1alpha1.Identity{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "app"},
			Status:     cloudv1alpha1.IdentityStatus{Conditions: ready(metav1.ConditionTrue, "Provisioned")},
		},
		&cloudv1alpha1.Identity{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "new"}},
		&cloudv1alpha1.Bucket{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "files"},
			Status: cloudv1alpha1.BucketStatus{
				Conditions: ready(metav1.ConditionFalse, "IAMFailed"),
				Bindings:   []cloudv1alpha1.BucketBinding{{Role: "roles/storage.objectAdmin", Member: "serviceAccount:x"}},
			},
		},
	}
	reader := clientfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	want := `
# HELP tender_bucket_bindings IAM bindings tender has granted on the bucket.
# TYPE tender_bucket_bindings gauge
tender_bucket_bindings{name="files",namespace="shop"} 1
# HELP tender_resource_ready Whether the resource's Ready condition is True (1) or not (0); reason is the condition's reason.
# TYPE tender_resource_ready gauge
tender_resource_ready{kind="Bucket",name="files",namespace="shop",reason="IAMFailed"} 0
tender_resource_ready{kind="Identity",name="app",namespace="shop",reason="Provisioned"} 1
tender_resource_ready{kind="Identity",name="new",namespace="shop",reason="Pending"} 0
`
	if err := testutil.CollectAndCompare(NewCollector(reader), strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}

func TestInstrument(t *testing.T) {
	cloudRequests.Reset()
	cloudDuration.Reset()
	ctx := context.Background()
	f := fake.New("p")
	p := Instrument(f)

	if err := p.CreateBucket(ctx, cloud.BucketSpec{Name: "b", Location: "EU"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetBucket(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetBucket(ctx, "missing"); !errors.Is(err, cloud.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	f.Err = errors.New("boom")
	if _, err := p.CreateKey(ctx, "sa@p.iam.gserviceaccount.com"); err == nil {
		t.Fatal("expected an error")
	}

	for _, c := range []struct {
		op, result string
		want       float64
	}{
		{"CreateBucket", "ok", 1},
		{"GetBucket", "ok", 1},
		{"GetBucket", "not_found", 1},
		{"CreateKey", "error", 1},
	} {
		if got := testutil.ToFloat64(cloudRequests.WithLabelValues(c.op, c.result)); got != c.want {
			t.Errorf("%s/%s = %v, want %v", c.op, c.result, got, c.want)
		}
	}
	if n := testutil.CollectAndCount(cloudDuration); n != 3 {
		t.Errorf("duration series = %d, want 3", n)
	}
}
