package tenant

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Role names inside a tenant namespace.
const (
	// RoleAdmin can manage everything a tenant is allowed to manage.
	RoleAdmin = "tenant-admin"
	// RoleViewer can read it.
	RoleViewer = "tenant-viewer"
)

// ServiceAccountCI is the identity the generated kubeconfig authenticates as.
const ServiceAccountCI = "tenant-ci"

// KubeconfigSecret holds that kubeconfig.
const KubeconfigSecret = "tenant-kubeconfig"

// GroupPrefix maps a tenant to the OIDC group that administers it. The provider
// issues groups; this is the name a binding expects to see in a token.
const GroupPrefix = "paas:tenant:"

// tenantAPIGroups are the groups a tenant may manage inside its own namespace.
//
// Deliberately not "*": a tenant administering its own namespace must not be
// able to write RBAC in it, because granting itself more than its own role is
// the whole game. Roles and bindings are the platform's to write.
var tenantAPIGroups = []string{"", "apps", "batch", "networking.k8s.io", "apps.paas.io", "core.paas.io"}

func rules(verbs []string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups: tenantAPIGroups,
		Resources: []string{"*"},
		Verbs:     verbs,
	}}
}

func role(namespace, tenant, name string, verbs []string) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{TenantLabel: tenant},
		},
		Rules: rules(verbs),
	}
}

// binding grants a role to the named users, the tenant's OIDC group, and — for
// the admin role — the CI service account.
func binding(namespace, tenant, roleName string, subjects []rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name: roleName, Namespace: namespace,
			Labels: map[string]string{TenantLabel: tenant},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     roleName,
		},
		Subjects: subjects,
	}
}

// adminSubjects are the identities that administer a namespace: the tenant's own
// admins, every ancestor's admins, the tenant's OIDC group, and the CI account.
//
// Ancestors are included because administration flows down — ADR 0004. It does
// not flow up, which is why this takes the chain rather than the whole tree.
func adminSubjects(namespace string, admins []string, groups []string) []rbacv1.Subject {
	subjects := make([]rbacv1.Subject, 0, len(admins)+len(groups)+1)
	for _, user := range admins {
		subjects = append(subjects, rbacv1.Subject{
			APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user,
		})
	}
	for _, group := range groups {
		subjects = append(subjects, rbacv1.Subject{
			APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: GroupPrefix + group,
		})
	}
	return append(subjects, rbacv1.Subject{
		Kind: rbacv1.ServiceAccountKind, Name: ServiceAccountCI, Namespace: namespace,
	})
}
