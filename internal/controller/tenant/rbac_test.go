package tenant

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	rbacv1 "k8s.io/api/rbac/v1"
)

// A tenant administering its own namespace must not be able to write RBAC in
// it: granting itself more than its own role is the whole game.
func TestTenantRoles_CannotWriteRBAC(t *testing.T) {
	t.Parallel()

	admin := role("tenant-acme", "acme", RoleAdmin, []string{"*"})
	for _, rule := range admin.Rules {
		for _, group := range rule.APIGroups {
			if group == "rbac.authorization.k8s.io" || group == "*" {
				t.Errorf("tenant-admin grants %q, which lets a tenant escalate itself", group)
			}
		}
	}
}

func TestTenantRoles_ViewerIsReadOnly(t *testing.T) {
	t.Parallel()

	viewer := role("tenant-acme", "acme", RoleViewer, []string{"get", "list", "watch"})
	want := []string{"get", "list", "watch"}
	for _, rule := range viewer.Rules {
		if diff := cmp.Diff(want, rule.Verbs); diff != "" {
			t.Errorf("viewer verbs differ (-want +got):\n%s", diff)
		}
	}
}

// Administration flows down: a parent's admins administer descendants. The
// subjects a binding gets are what makes that true.
func TestAdminSubjects_CarriesUsersGroupsAndCI(t *testing.T) {
	t.Parallel()

	subjects := adminSubjects("tenant-acme-beta", []string{"alice@acme.com"}, []string{"beta", "acme"})

	var users, groups, accounts []string
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			users = append(users, s.Name)
		case rbacv1.GroupKind:
			groups = append(groups, s.Name)
		case rbacv1.ServiceAccountKind:
			accounts = append(accounts, s.Namespace+"/"+s.Name)
		}
	}

	if diff := cmp.Diff([]string{"alice@acme.com"}, users); diff != "" {
		t.Errorf("users differ (-want +got):\n%s", diff)
	}
	// Both the tenant's own group and its ancestor's, so a parent admin group
	// administers the child.
	if diff := cmp.Diff([]string{GroupPrefix + "beta", GroupPrefix + "acme"}, groups); diff != "" {
		t.Errorf("groups differ (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"tenant-acme-beta/" + ServiceAccountCI}, accounts); diff != "" {
		t.Errorf("service accounts differ (-want +got):\n%s", diff)
	}
}

func TestBinding_ReferencesTheRoleItNames(t *testing.T) {
	t.Parallel()

	b := binding("tenant-acme", "acme", RoleAdmin, nil)
	if b.RoleRef.Kind != "Role" || b.RoleRef.Name != RoleAdmin {
		t.Errorf("roleRef = %+v, want the namespaced %s role", b.RoleRef, RoleAdmin)
	}
}
