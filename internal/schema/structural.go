// Package schema converts a chart's values.schema.json into the structural
// schema a CustomResourceDefinition needs.
package schema

import (
	"encoding/json"
	"fmt"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// UnrepresentableError reports a schema Kubernetes structural schemas cannot
// express, and where in the document it is.
type UnrepresentableError struct {
	Path   string
	Reason string
}

// Error implements the error interface.
func (e *UnrepresentableError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// Convert turns a JSON Schema document into a structural schema.
//
// It returns an error rather than a partial result for anything it cannot
// represent. Because a tenant CR's spec is passed to Helm as values, a dropped
// constraint is not a missing validation — it is an unvalidated field reaching
// the chart.
func Convert(raw []byte) (*apiextensionsv1.JSONSchemaProps, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse values.schema.json: %w", err)
	}
	return convert(doc, ".")
}

func convert(doc map[string]any, path string) (*apiextensionsv1.JSONSchemaProps, error) {
	prefix := path
	if prefix == "." {
		prefix = ""
	}

	for _, banned := range []string{"$ref", "definitions", "$defs", "patternProperties", "not", "dependencies"} {
		if _, ok := doc[banned]; ok {
			return nil, &UnrepresentableError{Path: path, Reason: banned + " is not expressible in a structural schema"}
		}
	}

	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		v, ok := doc[key]
		if !ok {
			continue
		}
		items, ok := v.([]any)
		if !ok {
			return nil, &UnrepresentableError{Path: path, Reason: key + " must be an array"}
		}
		for i, it := range items {
			sub, ok := it.(map[string]any)
			if !ok {
				return nil, &UnrepresentableError{Path: fmt.Sprintf("%s.%s[%d]", prefix, key, i), Reason: "must be an object"}
			}
			for _, banned := range []string{"type", "additionalProperties", "default", "nullable"} {
				if _, bad := sub[banned]; bad {
					return nil, &UnrepresentableError{
						Path:   fmt.Sprintf("%s.%s[%d]", prefix, key, i),
						Reason: banned + " may not appear inside " + key,
					}
				}
			}
		}
		return nil, &UnrepresentableError{Path: path, Reason: key + " is not supported by the generator"}
	}

	typ, _ := doc["type"].(string)
	if typ == "" {
		return nil, &UnrepresentableError{Path: path, Reason: "every subschema needs an explicit type"}
	}
	if path == "." && typ != "object" {
		return nil, &UnrepresentableError{Path: path, Reason: "the root of values.schema.json must be an object"}
	}

	out := &apiextensionsv1.JSONSchemaProps{Type: typ}
	if d, ok := doc["description"].(string); ok {
		out.Description = d
	}
	if n, ok := doc["minimum"].(float64); ok {
		out.Minimum = &n
	}
	if n, ok := doc["maximum"].(float64); ok {
		out.Maximum = &n
	}
	if s, ok := doc["pattern"].(string); ok {
		out.Pattern = s
	}
	if v, ok := doc["enum"]; ok {
		items, ok := v.([]any)
		if !ok {
			return nil, &UnrepresentableError{Path: path, Reason: "enum must be an array"}
		}
		for _, it := range items {
			// it came from json.Unmarshal into `any`, so re-marshaling cannot fail.
			b, _ := json.Marshal(it)
			out.Enum = append(out.Enum, apiextensionsv1.JSON{Raw: b})
		}
	}
	if v, ok := doc["default"]; ok {
		b, _ := json.Marshal(v)
		out.Default = &apiextensionsv1.JSON{Raw: b}
	}
	if v, ok := doc["required"]; ok {
		items, ok := v.([]any)
		if !ok {
			return nil, &UnrepresentableError{Path: path, Reason: "required must be an array"}
		}
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return nil, &UnrepresentableError{Path: path, Reason: "required entries must be strings"}
			}
			out.Required = append(out.Required, s)
		}
	}

	if props, ok := doc["properties"].(map[string]any); ok {
		out.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
		names := make([]string, 0, len(props))
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			sub, ok := props[k].(map[string]any)
			if !ok {
				return nil, &UnrepresentableError{Path: prefix + ".properties." + k, Reason: "must be an object"}
			}
			conv, err := convert(sub, prefix+".properties."+k)
			if err != nil {
				return nil, err
			}
			out.Properties[k] = *conv
		}
	}

	if items, ok := doc["items"].(map[string]any); ok {
		conv, err := convert(items, prefix+".items")
		if err != nil {
			return nil, err
		}
		out.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: conv}
	}

	return out, nil
}
