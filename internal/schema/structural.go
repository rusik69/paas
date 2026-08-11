// Package schema converts a chart's values.schema.json into the structural
// schema a CustomResourceDefinition needs.
package schema

import (
	"encoding/json"
	"fmt"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// isKnownKey reports whether convert understands a JSON Schema keyword,
// either by extracting it or by rejecting it outright (the banned and
// combinator keys, handled before this is consulted). A keyword that is
// present in a document but not recognised here is what would otherwise be
// silently dropped — this is what keeps the converter fail-closed.
func isKnownKey(key string) bool {
	switch key {
	case "$ref", "definitions", "$defs", "patternProperties", "not", "dependencies",
		"oneOf", "anyOf", "allOf",
		"type", "description", "minimum", "maximum", "pattern",
		"enum", "default", "required", "properties", "items",
		"maxLength", "minLength", "minItems", "maxItems",
		"minProperties", "maxProperties", "uniqueItems", "multipleOf",
		"exclusiveMinimum", "exclusiveMaximum", "format", "title",
		"nullable", "additionalProperties":
		return true
	default:
		return false
	}
}

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
	var err error
	if out.Description, err = stringField(doc, "description", path); err != nil {
		return nil, err
	}
	if out.Pattern, err = stringField(doc, "pattern", path); err != nil {
		return nil, err
	}
	if out.Format, err = stringField(doc, "format", path); err != nil {
		return nil, err
	}
	if out.Title, err = stringField(doc, "title", path); err != nil {
		return nil, err
	}
	if out.UniqueItems, err = boolField(doc, "uniqueItems", path); err != nil {
		return nil, err
	}
	if out.ExclusiveMinimum, err = boolField(doc, "exclusiveMinimum", path); err != nil {
		return nil, err
	}
	if out.ExclusiveMaximum, err = boolField(doc, "exclusiveMaximum", path); err != nil {
		return nil, err
	}
	if out.Nullable, err = boolField(doc, "nullable", path); err != nil {
		return nil, err
	}
	if out.Minimum, err = floatField(doc, "minimum", path); err != nil {
		return nil, err
	}
	if out.Maximum, err = floatField(doc, "maximum", path); err != nil {
		return nil, err
	}
	if out.MultipleOf, err = floatField(doc, "multipleOf", path); err != nil {
		return nil, err
	}
	if out.MaxLength, err = intField(doc, "maxLength", path); err != nil {
		return nil, err
	}
	if out.MinLength, err = intField(doc, "minLength", path); err != nil {
		return nil, err
	}
	if out.MaxItems, err = intField(doc, "maxItems", path); err != nil {
		return nil, err
	}
	if out.MinItems, err = intField(doc, "minItems", path); err != nil {
		return nil, err
	}
	if out.MaxProperties, err = intField(doc, "maxProperties", path); err != nil {
		return nil, err
	}
	if out.MinProperties, err = intField(doc, "minProperties", path); err != nil {
		return nil, err
	}
	if av, ok := doc["additionalProperties"]; ok {
		if _, hasProps := doc["properties"]; hasProps {
			return nil, &UnrepresentableError{Path: path, Reason: "additionalProperties may not appear alongside properties"}
		}
		sub, ok := av.(map[string]any)
		if !ok {
			return nil, &UnrepresentableError{Path: path, Reason: "additionalProperties must be an object schema"}
		}
		conv, err := convert(sub, prefix+".additionalProperties")
		if err != nil {
			return nil, err
		}
		out.AdditionalProperties = &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true, Schema: conv}
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

	if v, ok := doc["properties"]; ok {
		props, ok := v.(map[string]any)
		if !ok {
			return nil, &UnrepresentableError{Path: path, Reason: "properties must be an object"}
		}
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

	if v, ok := doc["items"]; ok {
		items, ok := v.(map[string]any)
		if !ok {
			if _, isTuple := v.([]any); isTuple {
				return nil, &UnrepresentableError{Path: path, Reason: "tuple-form items is not expressible in a structural schema"}
			}
			return nil, &UnrepresentableError{Path: path, Reason: "items must be an object"}
		}
		conv, err := convert(items, prefix+".items")
		if err != nil {
			return nil, err
		}
		out.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: conv}
	}

	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if path == "." && k == "$schema" {
			continue
		}
		if !isKnownKey(k) {
			return nil, &UnrepresentableError{Path: path, Reason: fmt.Sprintf("%q is not a recognised schema keyword", k)}
		}
	}

	return out, nil
}

// intField reads an integer-valued keyword from a decoded JSON document.
// JSON numbers decode as float64, so a non-integral value is rejected rather
// than silently truncated.
func intField(doc map[string]any, key, path string) (*int64, error) {
	v, ok := doc[key]
	if !ok {
		return nil, nil
	}
	n, ok := v.(float64)
	if !ok {
		return nil, &UnrepresentableError{Path: path, Reason: key + " must be a number"}
	}
	if n != float64(int64(n)) {
		return nil, &UnrepresentableError{Path: path, Reason: key + " must be an integer"}
	}
	i := int64(n)
	return &i, nil
}

// floatField reads a number-valued keyword. A present value of the wrong
// type is rejected rather than left unset, so the constraint it names is
// never silently dropped.
func floatField(doc map[string]any, key, path string) (*float64, error) {
	v, ok := doc[key]
	if !ok {
		return nil, nil
	}
	n, ok := v.(float64)
	if !ok {
		return nil, &UnrepresentableError{Path: path, Reason: key + " must be a number"}
	}
	return &n, nil
}

// stringField reads a string-valued keyword, rejecting a present value of
// the wrong type instead of leaving it unset.
func stringField(doc map[string]any, key, path string) (string, error) {
	v, ok := doc[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &UnrepresentableError{Path: path, Reason: key + " must be a string"}
	}
	return s, nil
}

// boolField reads a boolean-valued keyword, rejecting a present value of
// the wrong type instead of leaving it unset.
func boolField(doc map[string]any, key, path string) (bool, error) {
	v, ok := doc[key]
	if !ok {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, &UnrepresentableError{Path: path, Reason: key + " must be a bool"}
	}
	return b, nil
}
