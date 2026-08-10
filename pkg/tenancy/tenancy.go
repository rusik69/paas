// Package tenancy interprets the tenant tree.
//
// It exists so the ancestor walk lives in exactly one place. ADR 0004 names
// reimplementing it per controller as the main hazard of the hierarchy: two
// controllers that disagree about which ancestor's monitoring serves a
// namespace will send a workload's metrics to different places, and neither is
// obviously wrong.
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/rusik69/paas/api/core/v1alpha1"
)

// Prefix distinguishes tenant namespaces from every other namespace in the
// cluster. It is part of the naming contract, not a cosmetic choice: the
// isolation policies select on it.
const Prefix = "tenant-"

// RootNamespace is where top-level tenants live.
const RootNamespace = Prefix + "root"

// MaxNamespaceLength is the DNS label limit a namespace name must fit.
const MaxNamespaceLength = 63

// ErrTooLong means the derived namespace name exceeded the label limit.
//
// Returned rather than truncated: two tenants whose paths differ only past the
// limit would otherwise collide on one namespace, and the second would
// silently administer the first's workloads.
var ErrTooLong = errors.New("derived namespace name exceeds " +
	fmt.Sprint(MaxNamespaceLength) + " characters")

// ErrEmptyPath means no tenant was named.
var ErrEmptyPath = errors.New("tenant path is empty")

// A DNS label, which is what each path segment has to be for the join to be a
// legal namespace name.
var label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// NamespaceFor derives the namespace backing a tenant at the given path.
//
// A child beta under acme is tenant-acme-beta.
func NamespaceFor(path []string) (string, error) {
	if len(path) == 0 {
		return "", ErrEmptyPath
	}
	for _, segment := range path {
		if !label.MatchString(segment) {
			return "", fmt.Errorf("tenant name %q is not a DNS label", segment)
		}
	}

	name := Prefix + strings.Join(path, "-")
	if len(name) > MaxNamespaceLength {
		return "", fmt.Errorf("%w: %s is %d", ErrTooLong, name, len(name))
	}
	return name, nil
}

// PathOf derives a tenant's path from the namespace it lives in.
//
// The tree is expressed by containment: a Tenant in tenant-acme is a child of
// acme, so the parent's path is recoverable from the namespace name alone and
// no parent field can disagree with where the object actually is.
func PathOf(t *corev1alpha1.Tenant) ([]string, error) {
	if t.Name == "" {
		return nil, ErrEmptyPath
	}
	if t.Namespace == RootNamespace {
		return []string{t.Name}, nil
	}
	if !strings.HasPrefix(t.Namespace, Prefix) {
		return nil, fmt.Errorf("tenant %s/%s is not in a tenant namespace", t.Namespace, t.Name)
	}

	parent := strings.TrimPrefix(t.Namespace, Prefix)
	if parent == "" {
		return nil, fmt.Errorf("tenant %s/%s has an empty parent path", t.Namespace, t.Name)
	}
	return append(strings.Split(parent, "-"), t.Name), nil
}

// NamespaceOf derives the namespace a tenant's own children and workloads live
// in.
func NamespaceOf(t *corev1alpha1.Tenant) (string, error) {
	path, err := PathOf(t)
	if err != nil {
		return "", err
	}
	return NamespaceFor(path)
}

// ParentOf returns the tenant one level up, or false at the root.
func ParentOf(ctx context.Context, r client.Reader, t *corev1alpha1.Tenant) (*corev1alpha1.Tenant, bool, error) {
	if t.Namespace == RootNamespace {
		return nil, false, nil
	}

	path, err := PathOf(t)
	if err != nil {
		return nil, false, err
	}
	// The parent's own name is the last segment of the namespace it owns, and
	// it lives one level further up.
	parentPath := path[:len(path)-1]
	// Joined directly rather than through NamespaceFor: this is a prefix of a
	// namespace name that already fits, so it cannot fail the length check, and
	// routing it through a fallible call would add a branch nothing can reach.
	grandparent := RootNamespace
	if len(parentPath) > 1 {
		grandparent = Prefix + strings.Join(parentPath[:len(parentPath)-1], "-")
	}

	var parent corev1alpha1.Tenant
	key := types.NamespacedName{Namespace: grandparent, Name: parentPath[len(parentPath)-1]}
	if err := r.Get(ctx, key, &parent); err != nil {
		return nil, false, fmt.Errorf("get parent tenant %s: %w", key, err)
	}
	return &parent, true, nil
}

// Resolve returns the nearest tenant at or above t with module enabled.
//
// The second return is false when no ancestor enables it, which is a normal
// answer and not an error: a tenant with no monitoring anywhere above it simply
// has none.
func Resolve(ctx context.Context, r client.Reader, t *corev1alpha1.Tenant, module string) (*corev1alpha1.Tenant, bool, error) {
	// Unbounded on purpose, and it terminates: every step replaces t with a
	// tenant one path segment shorter, and ParentOf reports the root. A
	// containment tree cannot cycle, so there is no depth guard to write and
	// none that a test could reach.
	for {
		if m, ok := t.Spec.Modules[module]; ok && m.Enabled {
			return t, true, nil
		}
		parent, ok, err := ParentOf(ctx, r, t)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		t = parent
	}
}
