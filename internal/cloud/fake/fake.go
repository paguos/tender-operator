// Package fake is an in-memory cloud.Provider for tests.
package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/paguos/tender-operator/internal/cloud"
)

// Provider keeps buckets, IAM bindings, service accounts and keys in memory.
// Set Err to make every call fail, e.g. to test error handling.
type Provider struct {
	Project string
	Err     error

	mu       sync.Mutex
	buckets  map[string]*bucket
	accounts map[string]*account
	nextKey  int
}

type bucket struct {
	cloud.Bucket
	objects  int
	bindings map[string][]string // role -> members
}

type account struct {
	displayName string
	keys        []string
}

var _ cloud.Provider = (*Provider)(nil)

// New returns an empty fake for project.
func New(project string) *Provider {
	return &Provider{
		Project:  project,
		buckets:  map[string]*bucket{},
		accounts: map[string]*account{},
	}
}

func (p *Provider) lock() (func(), error) {
	p.mu.Lock()
	if p.Err != nil {
		p.mu.Unlock()
		return nil, p.Err
	}
	return p.mu.Unlock, nil
}

func notFound(kind, name string) error {
	return fmt.Errorf("%w: %s %q", cloud.ErrNotFound, kind, name)
}

func (p *Provider) GetBucket(_ context.Context, name string) (*cloud.Bucket, error) {
	unlock, err := p.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, ok := p.buckets[name]
	if !ok {
		return nil, notFound("bucket", name)
	}
	out := b.Bucket
	out.Labels = maps.Clone(b.Labels)
	return &out, nil
}

func (p *Provider) CreateBucket(_ context.Context, spec cloud.BucketSpec) error {
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, ok := p.buckets[spec.Name]; ok {
		return fmt.Errorf("bucket %q already exists", spec.Name)
	}
	p.buckets[spec.Name] = &bucket{
		Bucket: cloud.Bucket{
			Name:         spec.Name,
			Location:     spec.Location,
			StorageClass: spec.StorageClass,
			Labels:       maps.Clone(spec.Labels),
		},
		bindings: map[string][]string{},
	}
	return nil
}

func (p *Provider) UpdateBucket(_ context.Context, spec cloud.BucketSpec) error {
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	b, ok := p.buckets[spec.Name]
	if !ok {
		return notFound("bucket", spec.Name)
	}
	b.StorageClass = spec.StorageClass
	b.Labels = maps.Clone(spec.Labels)
	return nil
}

func (p *Provider) BucketEmpty(_ context.Context, name string) (bool, error) {
	unlock, err := p.lock()
	if err != nil {
		return false, err
	}
	defer unlock()
	b, ok := p.buckets[name]
	if !ok {
		return false, notFound("bucket", name)
	}
	return b.objects == 0, nil
}

func (p *Provider) DeleteBucket(_ context.Context, name string) error {
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	b, ok := p.buckets[name]
	if !ok {
		return notFound("bucket", name)
	}
	if b.objects > 0 {
		return fmt.Errorf("bucket %q is not empty", name)
	}
	delete(p.buckets, name)
	return nil
}

func (p *Provider) GrantBucketRole(_ context.Context, name, role, member string) error {
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	b, ok := p.buckets[name]
	if !ok {
		return notFound("bucket", name)
	}
	if !slices.Contains(b.bindings[role], member) {
		b.bindings[role] = append(b.bindings[role], member)
	}
	return nil
}

func (p *Provider) RevokeBucketRole(_ context.Context, name, role, member string) error {
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	b, ok := p.buckets[name]
	if !ok {
		return notFound("bucket", name)
	}
	b.bindings[role] = slices.DeleteFunc(b.bindings[role], func(m string) bool { return m == member })
	return nil
}

func (p *Provider) email(accountID string) string {
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", accountID, p.Project)
}

func (p *Provider) EnsureServiceAccount(_ context.Context, accountID, displayName string) (string, error) {
	unlock, err := p.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	email := p.email(accountID)
	if _, ok := p.accounts[email]; !ok {
		p.accounts[email] = &account{displayName: displayName}
	}
	return email, nil
}

func (p *Provider) DeleteServiceAccount(_ context.Context, email string) error {
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, ok := p.accounts[email]; !ok {
		return notFound("service account", email)
	}
	delete(p.accounts, email)
	return nil
}

func (p *Provider) ListKeys(_ context.Context, email string) ([]string, error) {
	unlock, err := p.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	a, ok := p.accounts[email]
	if !ok {
		return nil, notFound("service account", email)
	}
	return slices.Clone(a.keys), nil
}

func (p *Provider) CreateKey(_ context.Context, email string) (*cloud.Key, error) {
	unlock, err := p.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	a, ok := p.accounts[email]
	if !ok {
		return nil, notFound("service account", email)
	}
	if len(a.keys) >= 10 {
		return nil, fmt.Errorf("service account %q already has 10 keys", email)
	}
	p.nextKey++
	id := fmt.Sprintf("key-%d", p.nextKey)
	a.keys = append(a.keys, id)
	json := fmt.Sprintf(`{"type":"service_account","client_email":%q,"private_key_id":%q}`, email, id)
	return &cloud.Key{ID: id, JSON: []byte(json)}, nil
}

func (p *Provider) DeleteKey(_ context.Context, email, keyID string) error {
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	a, ok := p.accounts[email]
	if !ok {
		return notFound("service account", email)
	}
	i := slices.Index(a.keys, keyID)
	if i < 0 {
		return notFound("key", keyID)
	}
	a.keys = slices.Delete(a.keys, i, i+1)
	return nil
}

// Test helpers. They bypass Err.

// AddObjects pretends n objects were uploaded to the bucket.
func (p *Provider) AddObjects(name string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buckets[name].objects += n
}

// HasBucket reports whether the bucket exists.
func (p *Provider) HasBucket(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.buckets[name]
	return ok
}

// Bindings returns the bucket's non-empty bindings, role -> members.
func (p *Provider) Bindings(name string) map[string][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string][]string{}
	if b, ok := p.buckets[name]; ok {
		for r, m := range b.bindings {
			if len(m) > 0 {
				out[r] = slices.Clone(m)
			}
		}
	}
	return out
}

// Members returns the members bound to role on the bucket.
func (p *Provider) Members(name, role string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b, ok := p.buckets[name]; ok {
		return slices.Clone(b.bindings[role])
	}
	return nil
}

// HasAccount reports whether the service account exists.
func (p *Provider) HasAccount(email string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.accounts[email]
	return ok
}

// Keys returns the account's key IDs.
func (p *Provider) Keys(email string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.accounts[email]; ok {
		return slices.Clone(a.keys)
	}
	return nil
}
