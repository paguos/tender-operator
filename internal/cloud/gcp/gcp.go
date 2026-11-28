// Package gcp implements cloud.Provider on Google Cloud Storage, IAM and
// Resource Manager (project IAM policy).
//
// It uses the generated REST clients (google.golang.org/api) rather than the
// gRPC-based cloud.google.com/go/storage, which keeps the binary small.
package gcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"

	crm "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/googleapi"
	iamadmin "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	gcs "google.golang.org/api/storage/v1"

	"github.com/paguos/tender-operator/internal/cloud"
)

// Provider talks to Google Cloud in one project.
type Provider struct {
	project string
	storage *gcs.Service
	iam     *iamadmin.Service
	crm     *crm.Service
}

var _ cloud.Provider = (*Provider)(nil)

// New builds a Provider with Application Default Credentials
// (GOOGLE_APPLICATION_CREDENTIALS or the metadata server).
func New(ctx context.Context, project string, opts ...option.ClientOption) (*Provider, error) {
	sc, err := gcs.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	ic, err := iamadmin.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("iam client: %w", err)
	}
	rc, err := crm.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("resource manager client: %w", err)
	}
	return NewWithClients(project, sc, ic, rc), nil
}

// NewWithClients builds a Provider from existing clients (used in tests).
func NewWithClients(project string, sc *gcs.Service, ic *iamadmin.Service, rc *crm.Service) *Provider {
	return &Provider{project: project, storage: sc, iam: ic, crm: rc}
}

func (p *Provider) GetBucket(ctx context.Context, name string) (*cloud.Bucket, error) {
	b, err := p.storage.Buckets.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, mapErr(err)
	}
	return &cloud.Bucket{
		Name:         b.Name,
		Location:     b.Location,
		StorageClass: b.StorageClass,
		Labels:       b.Labels,
	}, nil
}

func (p *Provider) CreateBucket(ctx context.Context, spec cloud.BucketSpec) error {
	_, err := p.storage.Buckets.Insert(p.project, &gcs.Bucket{
		Name:         spec.Name,
		Location:     spec.Location,
		StorageClass: spec.StorageClass,
		Labels:       spec.Labels,
		IamConfiguration: &gcs.BucketIamConfiguration{
			UniformBucketLevelAccess: &gcs.BucketIamConfigurationUniformBucketLevelAccess{Enabled: true},
			PublicAccessPrevention:   "enforced",
		},
	}).Context(ctx).Do()
	return mapErr(err)
}

func (p *Provider) UpdateBucket(ctx context.Context, spec cloud.BucketSpec) error {
	cur, err := p.storage.Buckets.Get(spec.Name).Context(ctx).Do()
	if err != nil {
		return mapErr(err)
	}
	// In a PATCH, a label set to null is removed.
	patch := &gcs.Bucket{StorageClass: spec.StorageClass, Labels: maps.Clone(spec.Labels)}
	for k := range cur.Labels {
		if _, ok := spec.Labels[k]; !ok {
			patch.NullFields = append(patch.NullFields, "Labels."+k)
		}
	}
	_, err = p.storage.Buckets.Patch(spec.Name, patch).Context(ctx).Do()
	return mapErr(err)
}

func (p *Provider) BucketEmpty(ctx context.Context, name string) (bool, error) {
	objs, err := p.storage.Objects.List(name).Versions(true).MaxResults(1).Fields("items/name").Context(ctx).Do()
	if err != nil {
		return false, mapErr(err)
	}
	return len(objs.Items) == 0, nil
}

func (p *Provider) DeleteBucket(ctx context.Context, name string) error {
	return mapErr(p.storage.Buckets.Delete(name).Context(ctx).Do())
}

func (p *Provider) GrantBucketRole(ctx context.Context, bucket, role, member string) error {
	policy, err := p.storage.Buckets.GetIamPolicy(bucket).Context(ctx).Do()
	if err != nil {
		return mapErr(err)
	}
	for _, b := range policy.Bindings {
		if b.Role == role && b.Condition == nil {
			if slices.Contains(b.Members, member) {
				return nil
			}
			b.Members = append(b.Members, member)
			return p.setPolicy(ctx, bucket, policy)
		}
	}
	policy.Bindings = append(policy.Bindings, &gcs.PolicyBindings{Role: role, Members: []string{member}})
	return p.setPolicy(ctx, bucket, policy)
}

func (p *Provider) RevokeBucketRole(ctx context.Context, bucket, role, member string) error {
	policy, err := p.storage.Buckets.GetIamPolicy(bucket).Context(ctx).Do()
	if err != nil {
		return mapErr(err)
	}
	changed := false
	kept := policy.Bindings[:0]
	for _, b := range policy.Bindings {
		if b.Role == role && b.Condition == nil {
			if i := slices.Index(b.Members, member); i >= 0 {
				b.Members = slices.Delete(b.Members, i, i+1)
				changed = true
			}
			if len(b.Members) == 0 {
				continue
			}
		}
		kept = append(kept, b)
	}
	if !changed {
		return nil
	}
	policy.Bindings = kept
	return p.setPolicy(ctx, bucket, policy)
}

// setPolicy writes the policy back with its etag, so concurrent edits fail
// instead of being overwritten (the reconcile then retries).
func (p *Provider) setPolicy(ctx context.Context, bucket string, policy *gcs.Policy) error {
	_, err := p.storage.Buckets.SetIamPolicy(bucket, policy).Context(ctx).Do()
	return mapErr(err)
}

func (p *Provider) saName(email string) string {
	return fmt.Sprintf("projects/%s/serviceAccounts/%s", p.project, email)
}

func (p *Provider) EnsureServiceAccount(ctx context.Context, accountID, displayName string) (string, error) {
	email := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", accountID, p.project)
	sa, err := p.iam.Projects.ServiceAccounts.Get(p.saName(email)).Context(ctx).Do()
	if err == nil {
		return sa.Email, nil
	}
	if !isCode(err, http.StatusNotFound) {
		return "", err
	}
	sa, err = p.iam.Projects.ServiceAccounts.Create("projects/"+p.project, &iamadmin.CreateServiceAccountRequest{
		AccountId:      accountID,
		ServiceAccount: &iamadmin.ServiceAccount{DisplayName: displayName},
	}).Context(ctx).Do()
	if isCode(err, http.StatusConflict) {
		return email, nil
	}
	if err != nil {
		return "", err
	}
	return sa.Email, nil
}

func (p *Provider) DeleteServiceAccount(ctx context.Context, email string) error {
	_, err := p.iam.Projects.ServiceAccounts.Delete(p.saName(email)).Context(ctx).Do()
	return mapErr(err)
}

func (p *Provider) ListKeys(ctx context.Context, email string) ([]string, error) {
	resp, err := p.iam.Projects.ServiceAccounts.Keys.List(p.saName(email)).KeyTypes("USER_MANAGED").Context(ctx).Do()
	if err != nil {
		return nil, mapErr(err)
	}
	ids := make([]string, 0, len(resp.Keys))
	for _, k := range resp.Keys {
		ids = append(ids, path.Base(k.Name))
	}
	return ids, nil
}

func (p *Provider) CreateKey(ctx context.Context, email string) (*cloud.Key, error) {
	k, err := p.iam.Projects.ServiceAccounts.Keys.Create(p.saName(email), &iamadmin.CreateServiceAccountKeyRequest{}).Context(ctx).Do()
	if err != nil {
		return nil, mapErr(err)
	}
	data, err := base64.StdEncoding.DecodeString(k.PrivateKeyData)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	return &cloud.Key{ID: path.Base(k.Name), JSON: data}, nil
}

func (p *Provider) DeleteKey(ctx context.Context, email, keyID string) error {
	_, err := p.iam.Projects.ServiceAccounts.Keys.Delete(p.saName(email) + "/keys/" + keyID).Context(ctx).Do()
	return mapErr(err)
}

// GrantProjectRole adds member to an unconditional binding for role on the
// project. Conditional bindings are left alone.
func (p *Provider) GrantProjectRole(ctx context.Context, role, member string) error {
	policy, err := p.projectPolicy(ctx)
	if err != nil {
		return err
	}
	for _, b := range policy.Bindings {
		if b.Role == role && b.Condition == nil {
			if slices.Contains(b.Members, member) {
				return nil
			}
			b.Members = append(b.Members, member)
			return p.setProjectPolicy(ctx, policy)
		}
	}
	policy.Bindings = append(policy.Bindings, &crm.Binding{Role: role, Members: []string{member}})
	return p.setProjectPolicy(ctx, policy)
}

// RevokeProjectRole removes member from the unconditional binding for role.
func (p *Provider) RevokeProjectRole(ctx context.Context, role, member string) error {
	policy, err := p.projectPolicy(ctx)
	if err != nil {
		return err
	}
	changed := false
	kept := policy.Bindings[:0]
	for _, b := range policy.Bindings {
		if b.Role == role && b.Condition == nil {
			if i := slices.Index(b.Members, member); i >= 0 {
				b.Members = slices.Delete(b.Members, i, i+1)
				changed = true
			}
			if len(b.Members) == 0 {
				continue
			}
		}
		kept = append(kept, b)
	}
	if !changed {
		return nil
	}
	policy.Bindings = kept
	return p.setProjectPolicy(ctx, policy)
}

// projectPolicy reads the project policy at version 3, so conditional
// bindings come back intact and are written back unchanged.
func (p *Provider) projectPolicy(ctx context.Context) (*crm.Policy, error) {
	policy, err := p.crm.Projects.GetIamPolicy("projects/"+p.project, &crm.GetIamPolicyRequest{
		Options: &crm.GetPolicyOptions{RequestedPolicyVersion: 3},
	}).Context(ctx).Do()
	return policy, mapErr(err)
}

// setProjectPolicy writes the policy back with its etag, so a concurrent edit
// makes the call fail instead of being overwritten (the reconcile then retries).
func (p *Provider) setProjectPolicy(ctx context.Context, policy *crm.Policy) error {
	policy.Version = 3
	_, err := p.crm.Projects.SetIamPolicy("projects/"+p.project, &crm.SetIamPolicyRequest{Policy: policy}).Context(ctx).Do()
	return mapErr(err)
}

func isCode(err error, code int) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == code
}

// mapErr turns 404s into cloud.ErrNotFound.
func mapErr(err error) error {
	if isCode(err, http.StatusNotFound) {
		return fmt.Errorf("%w: %v", cloud.ErrNotFound, err)
	}
	return err
}
