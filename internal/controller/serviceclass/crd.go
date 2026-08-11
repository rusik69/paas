// Package serviceclass reconciles a ServiceClass into the CustomResourceDefinition
// that serves its kind.
package serviceclass

import (
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rusik69/paas/api/platform/v1alpha1"
	paasschema "github.com/rusik69/paas/internal/schema"
)

// Group and Version are the API the generated kinds are served under.
const (
	Group   = "apps.paas.io"
	Version = "v1alpha1"
)

// ManagedByLabel names the ServiceClass a generated CRD came from, so an
// orphaned CRD is identifiable after its class is gone.
const ManagedByLabel = "platform.paas.io/service-class"

// GVKFor is the group-version-kind a ServiceClass generates.
func GVKFor(sc *v1alpha1.ServiceClass) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Group, Version: Version, Kind: sc.Spec.Kind}
}

// CRDNameFor is the name of the CustomResourceDefinition a ServiceClass
// generates.
func CRDNameFor(sc *v1alpha1.ServiceClass) string {
	return sc.Spec.Plural + "." + Group
}

// CRDFor renders the CustomResourceDefinition for a ServiceClass.
//
// The chart's schema becomes .spec verbatim, which is what makes one
// values.schema.json the validation and the security boundary at once.
func CRDFor(sc *v1alpha1.ServiceClass, rawSchema []byte) (*apiextensionsv1.CustomResourceDefinition, error) {
	specSchema, err := paasschema.Convert(rawSchema)
	if err != nil {
		return nil, fmt.Errorf("chart %s:%s: %w", sc.Spec.Chart.Name, sc.Spec.Chart.Version, err)
	}

	columns := []apiextensionsv1.CustomResourceColumnDefinition{{
		Name:     "Ready",
		Type:     "string",
		JSONPath: `.status.conditions[?(@.type=="Ready")].status`,
	}}
	if len(sc.Spec.StatusFrom) > 0 {
		columns = append(columns, apiextensionsv1.CustomResourceColumnDefinition{
			Name:     "Primary",
			Type:     "string",
			JSONPath: sc.Spec.StatusFrom[0].Path,
		})
	}
	columns = append(columns, apiextensionsv1.CustomResourceColumnDefinition{
		Name:     "Age",
		Type:     "date",
		JSONPath: ".metadata.creationTimestamp",
	})

	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiextensionsv1.SchemeGroupVersion.String(),
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   CRDNameFor(sc),
			Labels: map[string]string{ManagedByLabel: sc.Name},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: Group,
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     sc.Spec.Kind,
				ListKind: sc.Spec.Kind + "List",
				Plural:   sc.Spec.Plural,
				Singular: strings.ToLower(sc.Spec.Kind),
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:                     Version,
				Served:                   true,
				Storage:                  true,
				Subresources:             &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				AdditionalPrinterColumns: columns,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec":   *specSchema,
							"status": statusSchema(),
						},
					},
				},
			}},
		},
	}, nil
}

// statusSchema is ours rather than the chart's: conditions plus whatever
// statusFrom copies in, which is free-form by construction.
//
// primary is hardcoded because the only catalog entry today (postgres)
// copies into .status.primary. A future service copying into a different
// path needs that path declared here too, or a structural schema silently
// drops the field on write.
func statusSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"observedGeneration": {Type: "integer", Format: "int64"},
			"primary":            {Type: "string"},
			"conditions": {
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{
					Schema: &apiextensionsv1.JSONSchemaProps{
						Type:     "object",
						Required: []string{"type", "status", "lastTransitionTime", "reason"},
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"type":               {Type: "string"},
							"status":             {Type: "string"},
							"observedGeneration": {Type: "integer", Format: "int64"},
							"lastTransitionTime": {Type: "string", Format: "date-time"},
							"reason":             {Type: "string"},
							"message":            {Type: "string"},
						},
					},
				},
			},
		},
	}
}
