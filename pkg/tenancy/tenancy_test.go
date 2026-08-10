package tenancy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
	"github.com/rusik69/paas/pkg/tenancy"
)

func TestNamespaceFor(t *testing.T) {
	t.Parallel()

	// 63 - len("tenant-") = 56 usable characters.
	exactly56 := strings.Repeat("a", 56)
	oneTooMany := strings.Repeat("a", 57)

	cases := []struct {
		name    string
		path    []string
		want    string
		wantErr error
	}{
		{name: "root tenant", path: []string{"acme"}, want: "tenant-acme"},
		{name: "child", path: []string{"acme", "beta"}, want: "tenant-acme-beta"},
		{name: "grandchild", path: []string{"acme", "beta", "web"}, want: "tenant-acme-beta-web"},
		{name: "digits and dashes", path: []string{"acme-2", "env-1"}, want: "tenant-acme-2-env-1"},
		{name: "exactly at the limit", path: []string{exactly56}, want: tenancy.Prefix + exactly56},
		{name: "one over the limit", path: []string{oneTooMany}, wantErr: tenancy.ErrTooLong},
		{name: "empty path", path: nil, wantErr: tenancy.ErrEmptyPath},
		{name: "uppercase is not a label", path: []string{"Acme"}},
		{name: "underscore is not a label", path: []string{"ac_me"}},
		{name: "leading dash is not a label", path: []string{"-acme"}},
		{name: "trailing dash is not a label", path: []string{"acme-"}},
		{name: "empty segment", path: []string{"acme", ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tenancy.NamespaceFor(tc.path)
			switch {
			case tc.want != "":
				if err != nil {
					t.Fatalf("NamespaceFor(%v) = %v, want %q", tc.path, err, tc.want)
				}
				if got != tc.want {
					t.Errorf("NamespaceFor(%v) = %q, want %q", tc.path, got, tc.want)
				}
				if len(got) > tenancy.MaxNamespaceLength {
					t.Errorf("%q is %d characters, over the limit", got, len(got))
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("NamespaceFor(%v) = %v, want %v", tc.path, err, tc.wantErr)
				}
			default:
				if err == nil {
					t.Errorf("NamespaceFor(%v) = %q, want an error", tc.path, got)
				}
			}
		})
	}
}

// Truncating would let two tenants whose paths differ only past the limit
// collide on one namespace, and the second would administer the first's
// workloads.
func TestNamespaceFor_DoesNotTruncate(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 60)
	if got, err := tenancy.NamespaceFor([]string{long}); err == nil {
		t.Errorf("NamespaceFor returned %q instead of refusing to truncate", got)
	}
}

func tenant(ns, name string, modules map[string]corev1alpha1.Module) *corev1alpha1.Tenant {
	return &corev1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1alpha1.TenantSpec{Plan: corev1alpha1.PlanBusiness, Modules: modules},
	}
}

func TestPathOf(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		tenant  *corev1alpha1.Tenant
		want    []string
		wantErr bool
	}{
		{
			name:   "root",
			tenant: tenant(tenancy.RootNamespace, "acme", nil),
			want:   []string{"acme"},
		},
		{
			name:   "child",
			tenant: tenant("tenant-acme", "beta", nil),
			want:   []string{"acme", "beta"},
		},
		{
			name:   "grandchild",
			tenant: tenant("tenant-acme-beta", "web", nil),
			want:   []string{"acme", "beta", "web"},
		},
		{
			name:    "not a tenant namespace",
			tenant:  tenant("default", "acme", nil),
			wantErr: true,
		},
		{
			name:    "no name",
			tenant:  tenant(tenancy.RootNamespace, "", nil),
			wantErr: true,
		},
		{
			name:    "bare prefix is not a parent",
			tenant:  tenant(tenancy.Prefix, "beta", nil),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tenancy.PathOf(tc.tenant)
			if tc.wantErr {
				if err == nil {
					t.Errorf("PathOf = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PathOf: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("path differs (-want +got):\n%s", diff)
			}
		})
	}
}

// Copies its inputs: the fake client writes resourceVersion onto the objects it
// is given, so parallel tests sharing one fixture race on it.
func treeReader(t *testing.T, objs ...*corev1alpha1.Tenant) client.Reader {
	t.Helper()

	s := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("build scheme: %v", err)
	}

	copies := make([]client.Object, len(objs))
	for i, o := range objs {
		copies[i] = o.DeepCopy()
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(copies...).Build()
}

// The behaviour ADR 0004 calls the point of the hierarchy: a child without a
// module uses the nearest ancestor that has one.
func TestResolve(t *testing.T) {
	t.Parallel()

	on := map[string]corev1alpha1.Module{"monitoring": {Enabled: true}}
	off := map[string]corev1alpha1.Module{"monitoring": {Enabled: false}}

	root := tenant(tenancy.RootNamespace, "acme", on)
	child := tenant("tenant-acme", "beta", off)
	grandchild := tenant("tenant-acme-beta", "web", nil)

	cases := []struct {
		name  string
		from  *corev1alpha1.Tenant
		want  string
		found bool
	}{
		{name: "enabled here", from: root, want: "acme", found: true},
		{name: "false means use the parent's", from: child, want: "acme", found: true},
		{name: "absent means use an ancestor's", from: grandchild, want: "acme", found: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := treeReader(t, root, child, grandchild)
			got, found, err := tenancy.Resolve(context.Background(), r, tc.from.DeepCopy(), "monitoring")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if found != tc.found {
				t.Fatalf("found = %t, want %t", found, tc.found)
			}
			if got.Name != tc.want {
				t.Errorf("resolved to %q, want %q", got.Name, tc.want)
			}
		})
	}
}

// A tenant with no monitoring anywhere above it simply has none. Reporting that
// as an error would make every caller special-case the ordinary case.
func TestResolve_NoAncestorEnablesIt(t *testing.T) {
	t.Parallel()

	root := tenant(tenancy.RootNamespace, "acme", nil)
	child := tenant("tenant-acme", "beta", nil)

	r := treeReader(t, root, child)
	got, found, err := tenancy.Resolve(context.Background(), r, child, "monitoring")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if found {
		t.Errorf("resolved to %q, want not found", got.Name)
	}
}

func TestResolve_ReportsAMissingAncestor(t *testing.T) {
	t.Parallel()

	// The child exists, its parent does not: a tree mid-deletion, or a tenant
	// created under a namespace nothing owns.
	orphan := tenant("tenant-acme", "beta", nil)

	r := treeReader(t, orphan)
	if _, _, err := tenancy.Resolve(context.Background(), r, orphan, "monitoring"); err == nil {
		t.Error("a missing parent was treated as the top of the tree")
	}
}

func TestNamespaceOf(t *testing.T) {
	t.Parallel()

	got, err := tenancy.NamespaceOf(tenant("tenant-acme", "beta", nil))
	if err != nil {
		t.Fatalf("NamespaceOf: %v", err)
	}
	if got != "tenant-acme-beta" {
		t.Errorf("NamespaceOf = %q, want tenant-acme-beta", got)
	}
}

func TestNamespaceOf_PropagatesADerivationFailure(t *testing.T) {
	t.Parallel()

	if _, err := tenancy.NamespaceOf(tenant("default", "acme", nil)); err == nil {
		t.Error("a tenant outside the tree produced a namespace")
	}
}

func TestParentOf_RootHasNoParent(t *testing.T) {
	t.Parallel()

	r := treeReader(t)
	if _, ok, err := tenancy.ParentOf(context.Background(), r, tenant(tenancy.RootNamespace, "acme", nil)); err != nil || ok {
		t.Errorf("ParentOf(root) = (ok %t, err %v), want (false, nil)", ok, err)
	}
}

func TestParentOf_GrandchildFindsItsParent(t *testing.T) {
	t.Parallel()

	root := tenant(tenancy.RootNamespace, "acme", nil)
	child := tenant("tenant-acme", "beta", nil)
	grandchild := tenant("tenant-acme-beta", "web", nil)

	r := treeReader(t, root, child, grandchild)
	got, ok, err := tenancy.ParentOf(context.Background(), r, grandchild)
	if err != nil || !ok {
		t.Fatalf("ParentOf = (ok %t, err %v)", ok, err)
	}
	if got.Name != "beta" || got.Namespace != "tenant-acme" {
		t.Errorf("parent = %s/%s, want tenant-acme/beta", got.Namespace, got.Name)
	}
}

func TestParentOf_RejectsATenantOutsideTheTree(t *testing.T) {
	t.Parallel()

	r := treeReader(t)
	if _, _, err := tenancy.ParentOf(context.Background(), r, tenant("default", "acme", nil)); err == nil {
		t.Error("a tenant outside the tree reported a parent")
	}
}
