package schema

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestConvert_PlainObject(t *testing.T) {
	got, err := Convert([]byte(`{
		"type": "object",
		"properties": {
			"instances": {"type": "integer", "minimum": 1},
			"size": {"type": "string"}
		},
		"required": ["instances"]
	}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got.Type != "object" {
		t.Errorf("Type = %q, want object", got.Type)
	}
	if _, ok := got.Properties["instances"]; !ok {
		t.Error("instances is missing from the converted schema")
	}
	if got.Properties["instances"].Minimum == nil || *got.Properties["instances"].Minimum != 1 {
		t.Error("minimum was not carried across — a dropped constraint is an unvalidated field")
	}
	if len(got.Required) != 1 || got.Required[0] != "instances" {
		t.Errorf("Required = %v, want [instances]", got.Required)
	}
}

func TestConvert_PreserveUnknownFieldsIsNeverSet(t *testing.T) {
	got, err := Convert([]byte(`{"type":"object","properties":{"a":{"type":"string"}}}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got.XPreserveUnknownFields != nil {
		t.Error("x-kubernetes-preserve-unknown-fields was set — that hands the whole boundary away")
	}
}

func TestConvert_Rejects(t *testing.T) {
	cases := []struct {
		name, json, wantPath, wantReason string
	}{
		{
			name:       "ref",
			json:       `{"type":"object","properties":{"a":{"$ref":"#/definitions/x"}}}`,
			wantPath:   ".properties.a",
			wantReason: "$ref",
		},
		{
			name:       "definitions",
			json:       `{"type":"object","definitions":{"x":{"type":"string"}},"properties":{}}`,
			wantPath:   ".",
			wantReason: "definitions",
		},
		{
			name:       "patternProperties",
			json:       `{"type":"object","patternProperties":{"^a":{"type":"string"}}}`,
			wantPath:   ".",
			wantReason: "patternProperties",
		},
		{
			name:       "typed oneOf",
			json:       `{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}`,
			wantPath:   ".properties.a.oneOf[0]",
			wantReason: "type",
		},
		{
			name:       "untyped property",
			json:       `{"type":"object","properties":{"a":{"minimum":1}}}`,
			wantPath:   ".properties.a",
			wantReason: "type",
		},
		{
			name:       "oneOf not an array",
			json:       `{"type":"object","properties":{"a":{"oneOf":"nope"}}}`,
			wantPath:   ".properties.a",
			wantReason: "array",
		},
		{
			name:       "oneOf entry not an object",
			json:       `{"type":"object","properties":{"a":{"oneOf":[1,2]}}}`,
			wantPath:   ".properties.a.oneOf[0]",
			wantReason: "object",
		},
		{
			name:       "additionalProperties inside anyOf",
			json:       `{"type":"object","properties":{"a":{"anyOf":[{"additionalProperties":true}]}}}`,
			wantPath:   ".properties.a.anyOf[0]",
			wantReason: "additionalProperties",
		},
		{
			name:       "default inside allOf",
			json:       `{"type":"object","properties":{"a":{"allOf":[{"default":"x"}]}}}`,
			wantPath:   ".properties.a.allOf[0]",
			wantReason: "default",
		},
		{
			name:       "nullable inside oneOf",
			json:       `{"type":"object","properties":{"a":{"oneOf":[{"nullable":true}]}}}`,
			wantPath:   ".properties.a.oneOf[0]",
			wantReason: "nullable",
		},
		{
			name:       "oneOf without banned inner keys is still refused",
			json:       `{"type":"object","properties":{"a":{"oneOf":[{"minimum":1},{"maximum":2}]}}}`,
			wantPath:   ".properties.a",
			wantReason: "oneOf is not supported",
		},
		{
			name:       "required is not an array",
			json:       `{"type":"object","required":"nope"}`,
			wantPath:   ".",
			wantReason: "required must be an array",
		},
		{
			name:       "required entry is not a string",
			json:       `{"type":"object","required":[1]}`,
			wantPath:   ".",
			wantReason: "required entries must be strings",
		},
		{
			name:       "property is not an object",
			json:       `{"type":"object","properties":{"a":"nope"}}`,
			wantPath:   ".properties.a",
			wantReason: "must be an object",
		},
		{
			name:       "enum is not an array",
			json:       `{"type":"object","properties":{"a":{"type":"string","enum":"nope"}}}`,
			wantPath:   ".properties.a",
			wantReason: "enum must be an array",
		},
		{
			name:       "nested items error propagates",
			json:       `{"type":"object","properties":{"a":{"type":"array","items":{"minimum":1}}}}`,
			wantPath:   ".properties.a.items",
			wantReason: "type",
		},
		{
			name:       "additionalProperties alongside properties",
			json:       `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"string"}}`,
			wantPath:   ".",
			wantReason: "additionalProperties",
		},
		{
			name:       "unrecognised keyword",
			json:       `{"type":"object","properties":{"a":{"type":"string","deprecated":true}}}`,
			wantPath:   ".properties.a",
			wantReason: "deprecated",
		},
		{
			name:       "non-integral maxLength",
			json:       `{"type":"object","properties":{"a":{"type":"string","maxLength":5.5}}}`,
			wantPath:   ".properties.a",
			wantReason: "maxLength",
		},
		{
			name:       "minLength is not a number",
			json:       `{"type":"object","properties":{"a":{"type":"string","minLength":"x"}}}`,
			wantPath:   ".properties.a",
			wantReason: "minLength must be a number",
		},
		{
			name:       "non-integral maxItems",
			json:       `{"type":"object","properties":{"a":{"type":"array","maxItems":2.5}}}`,
			wantPath:   ".properties.a",
			wantReason: "maxItems",
		},
		{
			name:       "non-integral minItems",
			json:       `{"type":"object","properties":{"a":{"type":"array","minItems":2.5}}}`,
			wantPath:   ".properties.a",
			wantReason: "minItems",
		},
		{
			name:       "non-integral maxProperties",
			json:       `{"type":"object","properties":{"a":{"type":"object","maxProperties":2.5}}}`,
			wantPath:   ".properties.a",
			wantReason: "maxProperties",
		},
		{
			name:       "non-integral minProperties",
			json:       `{"type":"object","properties":{"a":{"type":"object","minProperties":2.5}}}`,
			wantPath:   ".properties.a",
			wantReason: "minProperties",
		},
		{
			name:       "additionalProperties is not an object schema",
			json:       `{"type":"object","additionalProperties":true}`,
			wantPath:   ".",
			wantReason: "additionalProperties must be an object schema",
		},
		{
			name:       "additionalProperties inner error propagates",
			json:       `{"type":"object","additionalProperties":{"minimum":1}}`,
			wantPath:   ".additionalProperties",
			wantReason: "type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Convert([]byte(tc.json))
			if err == nil {
				t.Fatalf("Convert succeeded and returned %+v — a schema it cannot represent must produce no schema at all", got)
			}
			if got != nil {
				t.Error("Convert returned a schema alongside its error; a partial schema is the failure mode this exists to prevent")
			}
			var ue *UnrepresentableError
			if !errors.As(err, &ue) {
				t.Fatalf("err = %v, want an *UnrepresentableError naming the path", err)
			}
			if ue.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", ue.Path, tc.wantPath)
			}
			if !strings.Contains(ue.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to mention %q", ue.Reason, tc.wantReason)
			}
		})
	}
}

func TestConvert_RejectsWrongShapedValues(t *testing.T) {
	cases := []struct {
		name, json, wantPath, wantReason string
	}{
		{
			name:       "tuple-form items",
			json:       `{"type":"object","properties":{"a":{"type":"array","items":[{"type":"string"}]}}}`,
			wantPath:   ".properties.a",
			wantReason: "not expressible",
		},
		{
			name:       "properties as an array",
			json:       `{"type":"object","properties":["a"]}`,
			wantPath:   ".",
			wantReason: "properties must be an object",
		},
		{
			name:       "additionalProperties as a number",
			json:       `{"type":"object","additionalProperties":5}`,
			wantPath:   ".",
			wantReason: "additionalProperties must be an object schema",
		},
		{
			name:       "format as a number",
			json:       `{"type":"object","properties":{"a":{"type":"string","format":5}}}`,
			wantPath:   ".properties.a",
			wantReason: "format must be a string",
		},
		{
			name:       "title as a number",
			json:       `{"type":"object","properties":{"a":{"type":"string","title":123}}}`,
			wantPath:   ".properties.a",
			wantReason: "title must be a string",
		},
		{
			name:       "description as a number",
			json:       `{"type":"object","description":7}`,
			wantPath:   ".",
			wantReason: "description must be a string",
		},
		{
			name:       "uniqueItems as a string",
			json:       `{"type":"object","properties":{"a":{"type":"array","uniqueItems":"yes"}}}`,
			wantPath:   ".properties.a",
			wantReason: "uniqueItems must be a bool",
		},
		{
			name:       "nullable as a number",
			json:       `{"type":"object","properties":{"a":{"type":"string","nullable":1}}}`,
			wantPath:   ".properties.a",
			wantReason: "nullable must be a bool",
		},
		{
			name:       "minimum as a string",
			json:       `{"type":"object","properties":{"a":{"type":"integer","minimum":"1"}}}`,
			wantPath:   ".properties.a",
			wantReason: "minimum must be a number",
		},
		{
			name:       "maximum as a string",
			json:       `{"type":"object","properties":{"a":{"type":"integer","maximum":"1"}}}`,
			wantPath:   ".properties.a",
			wantReason: "maximum must be a number",
		},
		{
			name:       "pattern as a number",
			json:       `{"type":"object","properties":{"a":{"type":"string","pattern":7}}}`,
			wantPath:   ".properties.a",
			wantReason: "pattern must be a string",
		},
		{
			name:       "multipleOf as a string",
			json:       `{"type":"object","properties":{"a":{"type":"integer","multipleOf":"2"}}}`,
			wantPath:   ".properties.a",
			wantReason: "multipleOf must be a number",
		},
		{
			name:       "exclusiveMinimum as a number",
			json:       `{"type":"object","properties":{"a":{"type":"integer","exclusiveMinimum":1}}}`,
			wantPath:   ".properties.a",
			wantReason: "exclusiveMinimum must be a bool",
		},
		{
			name:       "exclusiveMaximum as a number",
			json:       `{"type":"object","properties":{"a":{"type":"integer","exclusiveMaximum":1}}}`,
			wantPath:   ".properties.a",
			wantReason: "exclusiveMaximum must be a bool",
		},
		{
			name:       "items neither object nor tuple",
			json:       `{"type":"object","properties":{"a":{"type":"array","items":5}}}`,
			wantPath:   ".properties.a",
			wantReason: "items must be an object",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Convert([]byte(tc.json))
			if err == nil {
				t.Fatalf("Convert succeeded and returned %+v — a wrong-shaped value must not be silently skipped", got)
			}
			if got != nil {
				t.Error("Convert returned a schema alongside its error; a partial schema is the failure mode this exists to prevent")
			}
			var ue *UnrepresentableError
			if !errors.As(err, &ue) {
				t.Fatalf("err = %v, want an *UnrepresentableError naming the path", err)
			}
			if ue.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", ue.Path, tc.wantPath)
			}
			if !strings.Contains(ue.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to mention %q", ue.Reason, tc.wantReason)
			}
		})
	}
}

func TestConvert_CarriesFieldConstraints(t *testing.T) {
	got, err := Convert([]byte(`{
		"type": "object",
		"properties": {
			"size": {
				"type": "string",
				"description": "instance size",
				"maximum": 10,
				"pattern": "^[a-z]+$",
				"enum": ["small", "large"],
				"default": "small"
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	size := got.Properties["size"]
	if size.Description != "instance size" {
		t.Errorf("Description = %q, want %q", size.Description, "instance size")
	}
	if size.Maximum == nil || *size.Maximum != 10 {
		t.Error("maximum was not carried across")
	}
	if size.Pattern != "^[a-z]+$" {
		t.Errorf("Pattern = %q, want %q", size.Pattern, "^[a-z]+$")
	}
	if len(size.Enum) != 2 {
		t.Fatalf("Enum = %v, want 2 entries", size.Enum)
	}
	if size.Default == nil || string(size.Default.Raw) != `"small"` {
		t.Error("default was not carried across")
	}
}

func TestConvert_ItemsAreConverted(t *testing.T) {
	got, err := Convert([]byte(`{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}}}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	tags := got.Properties["tags"]
	if tags.Items == nil || tags.Items.Schema == nil {
		t.Fatal("Items was not set on an array property")
	}
	if tags.Items.Schema.Type != "string" {
		t.Errorf("Items.Schema.Type = %q, want string", tags.Items.Schema.Type)
	}
}

func TestConvert_MaxLengthSurvivesRoundTrip(t *testing.T) {
	got, err := Convert([]byte(`{"type":"object","properties":{"a":{"type":"string","maxLength":5}}}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	a := got.Properties["a"]
	if a.MaxLength == nil || *a.MaxLength != 5 {
		t.Error("maxLength was not carried across — a dropped constraint is an unvalidated field")
	}
}

func TestConvert_CarriesRemainingConstraints(t *testing.T) {
	got, err := Convert([]byte(`{
		"type": "object",
		"properties": {
			"a": {
				"type": "string",
				"minLength": 1,
				"format": "hostname",
				"title": "A",
				"nullable": true
			},
			"b": {
				"type": "array",
				"minItems": 1,
				"maxItems": 3,
				"uniqueItems": true
			},
			"c": {
				"type": "object",
				"minProperties": 1,
				"maxProperties": 5
			},
			"d": {
				"type": "integer",
				"multipleOf": 2,
				"exclusiveMinimum": true,
				"exclusiveMaximum": true
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	a := got.Properties["a"]
	if a.MinLength == nil || *a.MinLength != 1 {
		t.Error("minLength was not carried across")
	}
	if a.Format != "hostname" {
		t.Errorf("Format = %q, want hostname", a.Format)
	}
	if a.Title != "A" {
		t.Errorf("Title = %q, want A", a.Title)
	}
	if !a.Nullable {
		t.Error("nullable was not carried across")
	}

	b := got.Properties["b"]
	if b.MinItems == nil || *b.MinItems != 1 {
		t.Error("minItems was not carried across")
	}
	if b.MaxItems == nil || *b.MaxItems != 3 {
		t.Error("maxItems was not carried across")
	}
	if !b.UniqueItems {
		t.Error("uniqueItems was not carried across")
	}

	c := got.Properties["c"]
	if c.MinProperties == nil || *c.MinProperties != 1 {
		t.Error("minProperties was not carried across")
	}
	if c.MaxProperties == nil || *c.MaxProperties != 5 {
		t.Error("maxProperties was not carried across")
	}

	d := got.Properties["d"]
	if d.MultipleOf == nil || *d.MultipleOf != 2 {
		t.Error("multipleOf was not carried across")
	}
	if !d.ExclusiveMinimum {
		t.Error("exclusiveMinimum was not carried across")
	}
	if !d.ExclusiveMaximum {
		t.Error("exclusiveMaximum was not carried across")
	}
}

func TestConvert_AdditionalPropertiesWithoutPropertiesConverts(t *testing.T) {
	got, err := Convert([]byte(`{"type":"object","additionalProperties":{"type":"string"}}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got.AdditionalProperties == nil || got.AdditionalProperties.Schema == nil {
		t.Fatal("AdditionalProperties was not set")
	}
	if got.AdditionalProperties.Schema.Type != "string" {
		t.Errorf("AdditionalProperties.Schema.Type = %q, want string", got.AdditionalProperties.Schema.Type)
	}
}

func TestConvert_SchemaKeywordAtRootIsIgnored(t *testing.T) {
	_, err := Convert([]byte(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
}

func TestUnrepresentableError_Error(t *testing.T) {
	err := &UnrepresentableError{Path: ".properties.a", Reason: "$ref is not expressible"}
	want := ".properties.a: $ref is not expressible"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestConvert_NotJSON(t *testing.T) {
	if _, err := Convert([]byte(`{not json`)); err == nil {
		t.Fatal("Convert accepted malformed JSON")
	}
}

func TestConvert_RootMustBeObject(t *testing.T) {
	_, err := Convert([]byte(`{"type":"string"}`))
	var ue *UnrepresentableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want an *UnrepresentableError", err)
	}
	if ue.Path != "." {
		t.Errorf("Path = %q, want .", ue.Path)
	}
}

// TestConvert_PostgresChartSchema runs the actual published
// packages/apps/postgres/values.schema.json through Convert. Without this, a
// schema mistake in that file only surfaces once a ServiceClass tries to
// generate a CRD from it on a running cluster.
//
// It checks more than field presence: a converter that silently dropped
// instances.maximum, the storage.class enum, or a resource pattern would
// leave every property named below in place while the CRD it produced no
// longer enforced the bound behind it — exactly the failure mode "failing
// closed on the schema" exists to prevent. So each constraint the chart
// relies on as its security boundary is checked for the specific value it
// must carry, not just for having survived at all.
func TestConvert_PostgresChartSchema(t *testing.T) {
	path := filepath.Join("..", "..", "packages", "apps", "postgres", "values.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	got, err := Convert(raw)
	if err != nil {
		t.Fatalf("Convert(%s) rejected the postgres chart's schema: %v", path, err)
	}

	instances, ok := got.Properties["instances"]
	if !ok {
		t.Fatal("converted schema has no \"instances\" property")
	}
	if instances.Minimum == nil || *instances.Minimum != 1 {
		t.Errorf("instances.Minimum = %v, want 1", instances.Minimum)
	}
	if instances.Maximum == nil || *instances.Maximum != 3 {
		t.Errorf("instances.Maximum = %v, want 3 — a dropped ceiling here is a tenant scheduling as many instances as they like", instances.Maximum)
	}

	storage, ok := got.Properties["storage"]
	if !ok {
		t.Fatal("converted schema has no \"storage\" property")
	}
	class, ok := storage.Properties["class"]
	if !ok {
		t.Fatal("converted schema has no \"storage.class\" property")
	}
	if diff := cmp.Diff([]string{"replicated-2", "replicated-3"}, enumStrings(t, class.Enum)); diff != "" {
		t.Errorf("storage.class enum diff (-want +got):\n%s", diff)
	}
	size, ok := storage.Properties["size"]
	if !ok {
		t.Fatal("converted schema has no \"storage.size\" property")
	}
	if want := `^([1-9][0-9]{0,2}Mi|[1-5]Gi)$`; size.Pattern != want {
		t.Errorf("storage.size.Pattern = %q, want %q — a dropped bound lets a tenant ask for an unbounded volume", size.Pattern, want)
	}

	resources, ok := got.Properties["resources"]
	if !ok {
		t.Fatal("converted schema has no \"resources\" property")
	}
	if want := `^([1-9]|[1-9][0-9]|[1-4][0-9]{2}|500)m$`; resources.Properties["cpu"].Pattern != want {
		t.Errorf("resources.cpu.Pattern = %q, want %q", resources.Properties["cpu"].Pattern, want)
	}
	if want := `^([1-9][0-9]{0,2}Mi|1Gi)$`; resources.Properties["memory"].Pattern != want {
		t.Errorf("resources.memory.Pattern = %q, want %q", resources.Properties["memory"].Pattern, want)
	}
}

// TestConvert_RedisChartSchema runs the actual published
// packages/apps/redis/values.schema.json through Convert, the same way
// TestConvert_PostgresChartSchema does for postgres — without this, a schema
// mistake in that file only surfaces once a ServiceClass tries to generate a
// CRD from it on a running cluster.
func TestConvert_RedisChartSchema(t *testing.T) {
	path := filepath.Join("..", "..", "packages", "apps", "redis", "values.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	got, err := Convert(raw)
	if err != nil {
		t.Fatalf("Convert(%s) rejected the redis chart's schema: %v", path, err)
	}

	storage, ok := got.Properties["storage"]
	if !ok {
		t.Fatal("converted schema has no \"storage\" property")
	}
	class, ok := storage.Properties["class"]
	if !ok {
		t.Fatal("converted schema has no \"storage.class\" property")
	}
	if diff := cmp.Diff([]string{"replicated-2", "replicated-3"}, enumStrings(t, class.Enum)); diff != "" {
		t.Errorf("storage.class enum diff (-want +got):\n%s", diff)
	}
	size, ok := storage.Properties["size"]
	if !ok {
		t.Fatal("converted schema has no \"storage.size\" property")
	}
	if want := `^([1-9][0-9]{0,2}Mi|[1-5]Gi)$`; size.Pattern != want {
		t.Errorf("storage.size.Pattern = %q, want %q — a dropped bound lets a tenant ask for an unbounded volume", size.Pattern, want)
	}

	resources, ok := got.Properties["resources"]
	if !ok {
		t.Fatal("converted schema has no \"resources\" property")
	}
	if want := `^([1-9]|[1-9][0-9]|[1-4][0-9]{2}|500)m$`; resources.Properties["cpu"].Pattern != want {
		t.Errorf("resources.cpu.Pattern = %q, want %q", resources.Properties["cpu"].Pattern, want)
	}
	if want := `^([1-9][0-9]{0,2}Mi|1Gi)$`; resources.Properties["memory"].Pattern != want {
		t.Errorf("resources.memory.Pattern = %q, want %q", resources.Properties["memory"].Pattern, want)
	}
}

// enumStrings unmarshals a converted schema's raw JSON enum values back into
// strings, so a test can compare them with cmp.Diff instead of poking at
// json.RawMessage bytes.
func enumStrings(t *testing.T, enum []apiextensionsv1.JSON) []string {
	t.Helper()
	out := make([]string, len(enum))
	for i, v := range enum {
		if err := json.Unmarshal(v.Raw, &out[i]); err != nil {
			t.Fatalf("unmarshal enum value %d: %v", i, err)
		}
	}
	return out
}
