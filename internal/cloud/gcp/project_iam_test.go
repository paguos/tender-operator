package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	crm "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/option"
)

// fakeCRM serves getIamPolicy and setIamPolicy for one project.
type fakeCRM struct {
	mu        sync.Mutex
	policy    crm.Policy
	versions  []int64 // requested policy versions on get
	sets      int
	setEtags  []string
	setPolicy crm.Policy
}

func (f *fakeCRM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "projects/test-project:getIamPolicy"):
		var req crm.GetIamPolicyRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Options != nil {
			f.versions = append(f.versions, req.Options.RequestedPolicyVersion)
		}
		_ = json.NewEncoder(w).Encode(f.policy)
	case strings.HasSuffix(r.URL.Path, "projects/test-project:setIamPolicy"):
		var req crm.SetIamPolicyRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.sets++
		f.setEtags = append(f.setEtags, req.Policy.Etag)
		f.setPolicy = *req.Policy
		f.policy = *req.Policy
		f.policy.Etag = "etag-next"
		_ = json.NewEncoder(w).Encode(f.policy)
	default:
		http.NotFound(w, r)
	}
}

func newCRMProvider(t *testing.T, f *fakeCRM) *Provider {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	rc, err := crm.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return NewWithClients("test-project", nil, nil, rc)
}

const (
	member = "serviceAccount:app@test-project.iam.gserviceaccount.com"
	role   = "roles/aiplatform.user"
)

func TestGrantProjectRoleKeepsConditionalBindings(t *testing.T) {
	cond := &crm.Binding{Role: role, Members: []string{"user:a@example.com"}, Condition: &crm.Expr{Title: "t", Expression: "true"}}
	f := &fakeCRM{policy: crm.Policy{Version: 3, Etag: "etag-1", Bindings: []*crm.Binding{cond}}}
	p := newCRMProvider(t, f)

	if err := p.GrantProjectRole(context.Background(), role, member); err != nil {
		t.Fatal(err)
	}
	if f.sets != 1 || f.setEtags[0] != "etag-1" || f.setPolicy.Version != 3 {
		t.Fatalf("sets = %d, etags = %v, version = %d", f.sets, f.setEtags, f.setPolicy.Version)
	}
	if !slices.Equal(f.versions, []int64{3}) {
		t.Errorf("requested versions = %v, want [3]", f.versions)
	}
	if len(f.setPolicy.Bindings) != 2 || f.setPolicy.Bindings[0].Condition == nil {
		t.Fatalf("bindings = %+v", f.setPolicy.Bindings)
	}
	if b := f.setPolicy.Bindings[1]; b.Role != role || b.Condition != nil || !slices.Equal(b.Members, []string{member}) {
		t.Errorf("new binding = %+v", b)
	}

	// Granting again is a no-op.
	if err := p.GrantProjectRole(context.Background(), role, member); err != nil {
		t.Fatal(err)
	}
	if f.sets != 1 {
		t.Errorf("sets = %d after a repeat grant, want 1", f.sets)
	}
}

func TestRevokeProjectRole(t *testing.T) {
	other := "user:b@example.com"
	f := &fakeCRM{policy: crm.Policy{Version: 1, Etag: "etag-1", Bindings: []*crm.Binding{
		{Role: role, Members: []string{other, member}},
		{Role: "roles/viewer", Members: []string{member}},
	}}}
	p := newCRMProvider(t, f)

	if err := p.RevokeProjectRole(context.Background(), role, member); err != nil {
		t.Fatal(err)
	}
	if len(f.setPolicy.Bindings) != 2 || !slices.Equal(f.setPolicy.Bindings[0].Members, []string{other}) {
		t.Fatalf("bindings = %+v", f.setPolicy.Bindings)
	}
	if !slices.Equal(f.setPolicy.Bindings[1].Members, []string{member}) {
		t.Errorf("other role touched: %+v", f.setPolicy.Bindings[1])
	}

	// Removing the last member drops the binding; a missing one is a no-op.
	if err := p.RevokeProjectRole(context.Background(), "roles/viewer", member); err != nil {
		t.Fatal(err)
	}
	if len(f.setPolicy.Bindings) != 1 {
		t.Errorf("bindings = %+v, want the viewer binding gone", f.setPolicy.Bindings)
	}
	sets := f.sets
	if err := p.RevokeProjectRole(context.Background(), "roles/viewer", member); err != nil {
		t.Fatal(err)
	}
	if f.sets != sets {
		t.Error("revoking a missing binding wrote the policy")
	}
}
