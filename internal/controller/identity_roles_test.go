package controller

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
)

const (
	aiUser   = "roles/aiplatform.user"
	logsView = "roles/logging.viewer"
)

func withRoles(roles ...string) func(*cloudv1alpha1.Identity) {
	return func(i *cloudv1alpha1.Identity) { i.Spec.ProjectRoles = roles }
}

func (e *env) setRoles(name string, roles ...string) {
	e.t.Helper()
	id := e.getIdentity(name)
	id.Spec.ProjectRoles = roles
	if err := e.kube.Update(e.ctx, id); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) hasProjectRole(role, email string) bool {
	return slices.Contains(e.cloud.ProjectMembers(role), "serviceAccount:"+email)
}

func TestIdentityGrantsProjectRoles(t *testing.T) {
	e := newEnv(t, newIdentity("app", withRoles(logsView, aiUser)))
	e.identity.AllowedProjectRoles = []string{aiUser, logsView}
	e.mustReconcileIdentity("app")

	id := e.getIdentity("app")
	if c := readyOf(id.Status.Conditions); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v", c)
	}
	for _, role := range []string{aiUser, logsView} {
		if !e.hasProjectRole(role, id.Status.ServiceAccount) {
			t.Errorf("%s not granted", role)
		}
	}
	if want := []string{aiUser, logsView}; !slices.Equal(id.Status.ProjectRoles, want) {
		t.Errorf("status.projectRoles = %v, want %v", id.Status.ProjectRoles, want)
	}

	// A no-op reconcile changes nothing.
	e.mustReconcileIdentity("app")
	if got := e.cloud.ProjectMembers(aiUser); len(got) != 1 {
		t.Errorf("members of %s = %v, want one", aiUser, got)
	}
}

func TestIdentityRejectsRoleNotAllowed(t *testing.T) {
	e := newEnv(t, newIdentity("app", withRoles(aiUser, "roles/owner")))
	e.identity.AllowedProjectRoles = []string{aiUser}
	if _, err := e.reconcileIdentity("app"); err == nil {
		t.Fatal("expected an error")
	}

	id := e.getIdentity("app")
	if c := readyOf(id.Status.Conditions); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "RoleNotAllowed" {
		t.Fatalf("Ready = %+v, want False/RoleNotAllowed", c)
	}
	// Nothing is granted, not even the allowed role.
	if got := e.cloud.ProjectMembers(aiUser); len(got) != 0 {
		t.Errorf("members of %s = %v, want none", aiUser, got)
	}
	if got := e.cloud.ProjectMembers("roles/owner"); len(got) != 0 {
		t.Errorf("roles/owner granted to %v", got)
	}
}

func TestIdentityNoAllowListGrantsNothing(t *testing.T) {
	e := newEnv(t, newIdentity("app", withRoles(aiUser)))
	if _, err := e.reconcileIdentity("app"); err == nil {
		t.Fatal("expected an error")
	}
	if got := e.cloud.ProjectMembers(aiUser); len(got) != 0 {
		t.Errorf("members of %s = %v, want none", aiUser, got)
	}
}

func TestIdentityRevokesRemovedProjectRole(t *testing.T) {
	e := newEnv(t, newIdentity("app", withRoles(aiUser, logsView)))
	e.identity.AllowedProjectRoles = []string{aiUser, logsView}
	e.mustReconcileIdentity("app")
	sa := e.getIdentity("app").Status.ServiceAccount

	e.setRoles("app", aiUser)
	e.mustReconcileIdentity("app")

	if e.hasProjectRole(logsView, sa) {
		t.Error("removed role still granted")
	}
	if !e.hasProjectRole(aiUser, sa) {
		t.Error("kept role revoked")
	}
	if got := e.getIdentity("app").Status.ProjectRoles; !slices.Equal(got, []string{aiUser}) {
		t.Errorf("status.projectRoles = %v", got)
	}
}

func TestIdentityLeavesOthersBindingsAlone(t *testing.T) {
	e := newEnv(t, newIdentity("app", withRoles(aiUser)))
	e.identity.AllowedProjectRoles = []string{aiUser}
	other := "serviceAccount:someone@test-project.iam.gserviceaccount.com"
	if err := e.cloud.GrantProjectRole(context.Background(), aiUser, other); err != nil {
		t.Fatal(err)
	}
	e.mustReconcileIdentity("app")
	e.setRoles("app")
	e.mustReconcileIdentity("app")

	if got := e.cloud.ProjectMembers(aiUser); !slices.Equal(got, []string{other}) {
		t.Errorf("members of %s = %v, want only %s", aiUser, got, other)
	}
}

func TestIdentityDeleteRevokesProjectRoles(t *testing.T) {
	e := newEnv(t, newIdentity("app", withRoles(aiUser)))
	e.identity.AllowedProjectRoles = []string{aiUser}
	e.mustReconcileIdentity("app")

	e.delete("app", &cloudv1alpha1.Identity{})
	e.mustReconcileIdentity("app")

	if got := e.cloud.ProjectMembers(aiUser); len(got) != 0 {
		t.Errorf("members of %s = %v after delete, want none", aiUser, got)
	}
	if !e.gone("app", &cloudv1alpha1.Identity{}) {
		t.Error("Identity still exists")
	}
}
