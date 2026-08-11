package schema

import (
	"errors"
	"strings"
	"testing"
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
