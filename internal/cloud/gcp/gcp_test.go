package gcp

import (
	"context"
	"errors"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
	"google.golang.org/api/option"
	gcs "google.golang.org/api/storage/v1"

	"github.com/paguos/tender-operator/internal/cloud"
)

// The storage half of the provider runs against fake-gcs-server over HTTP.
// IAM (service accounts, keys, bucket policies) has no fake server; the
// controller tests cover that logic through the in-memory provider.
func newStorageProvider(t *testing.T) (*Provider, *fakestorage.Server) {
	t.Helper()
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http", Host: "127.0.0.1", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	sc, err := gcs.NewService(context.Background(),
		option.WithEndpoint(srv.URL()+"/storage/v1/"),
		option.WithHTTPClient(srv.HTTPClient()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewWithClients("test-project", sc, nil, nil), srv
}

func TestBucketLifecycle(t *testing.T) {
	ctx := context.Background()
	p, srv := newStorageProvider(t)

	if _, err := p.GetBucket(ctx, "b1"); !errors.Is(err, cloud.ErrNotFound) {
		t.Fatalf("GetBucket on a missing bucket: %v", err)
	}

	spec := cloud.BucketSpec{Name: "b1", Location: "EU", StorageClass: "STANDARD",
		Labels: map[string]string{"managed-by": "tender"}}
	if err := p.CreateBucket(ctx, spec); err != nil {
		t.Fatal(err)
	}
	got, err := p.GetBucket(ctx, "b1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "b1" {
		t.Errorf("GetBucket = %+v", got)
	}

	empty, err := p.BucketEmpty(ctx, "b1")
	if err != nil || !empty {
		t.Fatalf("BucketEmpty = %v, %v", empty, err)
	}
	srv.CreateObject(fakestorage.Object{ObjectAttrs: fakestorage.ObjectAttrs{BucketName: "b1", Name: "o"}, Content: []byte("hi")})
	if empty, err := p.BucketEmpty(ctx, "b1"); err != nil || empty {
		t.Fatalf("BucketEmpty after upload = %v, %v", empty, err)
	}
	if err := srv.Client().Bucket("b1").Object("o").Delete(ctx); err != nil {
		t.Fatal(err)
	}

	if err := p.DeleteBucket(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetBucket(ctx, "b1"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("GetBucket after delete: %v", err)
	}
	if err := p.DeleteBucket(ctx, "b1"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("DeleteBucket twice: %v", err)
	}
}
