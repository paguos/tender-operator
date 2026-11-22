// Package metrics exposes tender's own Prometheus metrics, next to the ones
// controller-runtime already registers (reconciles, work queues, Go runtime):
//
//	tender_resource_ready{kind,namespace,name,reason}   1 if Ready, else 0
//	tender_bucket_bindings{namespace,name}               IAM bindings tender holds on the bucket
//	tender_cloud_requests_total{operation,result}        calls to the cloud provider
//	tender_cloud_request_duration_seconds{operation}     their latency
package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
	"github.com/paguos/tender-operator/internal/cloud"
)

// Register adds tender's metrics to controller-runtime's registry, which the
// manager serves on --metrics-bind-address. reader is normally the manager's cache.
func Register(reader client.Reader) error {
	return errors.Join(
		ctrlmetrics.Registry.Register(NewCollector(reader)),
		ctrlmetrics.Registry.Register(cloudRequests),
		ctrlmetrics.Registry.Register(cloudDuration),
	)
}

// Collector reports the state of every Identity and Bucket at scrape time, so
// deleted objects simply stop being reported.
type Collector struct {
	reader   client.Reader
	ready    *prometheus.Desc
	bindings *prometheus.Desc
}

func NewCollector(reader client.Reader) *Collector {
	return &Collector{
		reader: reader,
		ready: prometheus.NewDesc("tender_resource_ready",
			"Whether the resource's Ready condition is True (1) or not (0); reason is the condition's reason.",
			[]string{"kind", "namespace", "name", "reason"}, nil),
		bindings: prometheus.NewDesc("tender_bucket_bindings",
			"IAM bindings tender has granted on the bucket.",
			[]string{"namespace", "name"}, nil),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.ready
	ch <- c.bindings
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := ctrl.Log.WithName("metrics")

	var ids cloudv1alpha1.IdentityList
	if err := c.reader.List(ctx, &ids); err != nil {
		log.Error(err, "list identities")
	}
	for _, id := range ids.Items {
		c.emitReady(ch, "Identity", id.Namespace, id.Name, id.Status.Conditions)
	}

	var buckets cloudv1alpha1.BucketList
	if err := c.reader.List(ctx, &buckets); err != nil {
		log.Error(err, "list buckets")
	}
	for _, b := range buckets.Items {
		c.emitReady(ch, "Bucket", b.Namespace, b.Name, b.Status.Conditions)
		ch <- prometheus.MustNewConstMetric(c.bindings, prometheus.GaugeValue, float64(len(b.Status.Bindings)), b.Namespace, b.Name)
	}
}

func (c *Collector) emitReady(ch chan<- prometheus.Metric, kind, ns, name string, conds []metav1.Condition) {
	v, why := 0.0, "Pending" // not reconciled yet
	if cond := meta.FindStatusCondition(conds, "Ready"); cond != nil {
		why = cond.Reason
		if cond.Status == metav1.ConditionTrue {
			v = 1
		}
	}
	ch <- prometheus.MustNewConstMetric(c.ready, prometheus.GaugeValue, v, kind, ns, name, why)
}

var (
	cloudRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tender_cloud_requests_total",
		Help: "Calls to the cloud provider by operation and result (ok, not_found, error).",
	}, []string{"operation", "result"})
	cloudDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tender_cloud_request_duration_seconds",
		Help:    "Latency of calls to the cloud provider.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"operation"})
)

func observe(op string, start time.Time, err error) {
	result := "ok"
	switch {
	case errors.Is(err, cloud.ErrNotFound):
		result = "not_found"
	case err != nil:
		result = "error"
	}
	cloudRequests.WithLabelValues(op, result).Inc()
	cloudDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

func call[T any](op string, f func() (T, error)) (T, error) {
	start := time.Now()
	v, err := f()
	observe(op, start, err)
	return v, err
}

func call0(op string, f func() error) error {
	start := time.Now()
	err := f()
	observe(op, start, err)
	return err
}

// Instrument wraps a provider so every call is counted and timed.
func Instrument(p cloud.Provider) cloud.Provider { return &instrumented{p} }

type instrumented struct{ next cloud.Provider }

func (i *instrumented) GetBucket(ctx context.Context, name string) (*cloud.Bucket, error) {
	return call("GetBucket", func() (*cloud.Bucket, error) { return i.next.GetBucket(ctx, name) })
}

func (i *instrumented) CreateBucket(ctx context.Context, spec cloud.BucketSpec) error {
	return call0("CreateBucket", func() error { return i.next.CreateBucket(ctx, spec) })
}

func (i *instrumented) UpdateBucket(ctx context.Context, spec cloud.BucketSpec) error {
	return call0("UpdateBucket", func() error { return i.next.UpdateBucket(ctx, spec) })
}

func (i *instrumented) BucketEmpty(ctx context.Context, name string) (bool, error) {
	return call("BucketEmpty", func() (bool, error) { return i.next.BucketEmpty(ctx, name) })
}

func (i *instrumented) DeleteBucket(ctx context.Context, name string) error {
	return call0("DeleteBucket", func() error { return i.next.DeleteBucket(ctx, name) })
}

func (i *instrumented) GrantBucketRole(ctx context.Context, bucket, role, member string) error {
	return call0("GrantBucketRole", func() error { return i.next.GrantBucketRole(ctx, bucket, role, member) })
}

func (i *instrumented) RevokeBucketRole(ctx context.Context, bucket, role, member string) error {
	return call0("RevokeBucketRole", func() error { return i.next.RevokeBucketRole(ctx, bucket, role, member) })
}

func (i *instrumented) EnsureServiceAccount(ctx context.Context, accountID, displayName string) (string, error) {
	return call("EnsureServiceAccount", func() (string, error) { return i.next.EnsureServiceAccount(ctx, accountID, displayName) })
}

func (i *instrumented) DeleteServiceAccount(ctx context.Context, email string) error {
	return call0("DeleteServiceAccount", func() error { return i.next.DeleteServiceAccount(ctx, email) })
}

func (i *instrumented) ListKeys(ctx context.Context, email string) ([]string, error) {
	return call("ListKeys", func() ([]string, error) { return i.next.ListKeys(ctx, email) })
}

func (i *instrumented) CreateKey(ctx context.Context, email string) (*cloud.Key, error) {
	return call("CreateKey", func() (*cloud.Key, error) { return i.next.CreateKey(ctx, email) })
}

func (i *instrumented) DeleteKey(ctx context.Context, email, keyID string) error {
	return call0("DeleteKey", func() error { return i.next.DeleteKey(ctx, email, keyID) })
}
