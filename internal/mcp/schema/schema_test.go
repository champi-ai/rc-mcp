package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

const testSchema = `{
  "type": "object",
  "properties": {
    "clientId": { "type": "string" },
    "timeout":  { "type": "integer", "minimum": 1, "maximum": 300 },
    "mode":     { "type": "string", "enum": ["overwrite", "append"] },
    "env":      { "type": "object", "additionalProperties": { "type": "string" } },
    "fields":   { "type": "array", "items": { "type": "string", "enum": ["cpu", "memory"] } },
    "follow":   { "type": "boolean" }
  },
  "required": ["clientId"]
}`

func validateJSON(t *testing.T, args string) []ValidationError {
	t.Helper()
	return Validate(json.RawMessage(testSchema), json.RawMessage(args))
}

func TestValidate_OK(t *testing.T) {
	errs := validateJSON(t, `{"clientId":"dev-1","timeout":30,"mode":"append","env":{"A":"b"},"fields":["cpu"],"follow":true}`)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	errs := validateJSON(t, `{}`)
	if len(errs) != 1 || errs[0].Path != "/clientId" || !strings.Contains(errs[0].Message, "required") {
		t.Fatalf("errs = %+v", errs)
	}
}

func TestValidate_WrongTypes(t *testing.T) {
	errs := validateJSON(t, `{"clientId":42,"timeout":"soon","follow":"yes"}`)
	if len(errs) != 3 {
		t.Fatalf("want 3 type errors, got %+v", errs)
	}
}

func TestValidate_IntegerRejectsFloat(t *testing.T) {
	errs := validateJSON(t, `{"clientId":"d","timeout":1.5}`)
	if len(errs) != 1 || errs[0].Path != "/timeout" {
		t.Fatalf("errs = %+v", errs)
	}
}

func TestValidate_Bounds(t *testing.T) {
	if errs := validateJSON(t, `{"clientId":"d","timeout":0}`); len(errs) != 1 || !strings.Contains(errs[0].Message, ">= 1") {
		t.Fatalf("min: %+v", errs)
	}
	if errs := validateJSON(t, `{"clientId":"d","timeout":301}`); len(errs) != 1 || !strings.Contains(errs[0].Message, "<= 300") {
		t.Fatalf("max: %+v", errs)
	}
}

func TestValidate_Enum(t *testing.T) {
	errs := validateJSON(t, `{"clientId":"d","mode":"truncate"}`)
	if len(errs) != 1 || errs[0].Path != "/mode" || !strings.Contains(errs[0].Message, "one of") {
		t.Fatalf("errs = %+v", errs)
	}
}

func TestValidate_AdditionalPropertiesSchema(t *testing.T) {
	errs := validateJSON(t, `{"clientId":"d","env":{"A":1}}`)
	if len(errs) != 1 || errs[0].Path != "/env/A" {
		t.Fatalf("errs = %+v", errs)
	}
}

func TestValidate_ArrayItems(t *testing.T) {
	errs := validateJSON(t, `{"clientId":"d","fields":["cpu","disk"]}`)
	if len(errs) != 1 || errs[0].Path != "/fields/1" {
		t.Fatalf("errs = %+v", errs)
	}
}

func TestValidate_UnknownFieldsAllowedByDefault(t *testing.T) {
	if errs := validateJSON(t, `{"clientId":"d","extra":"whatever"}`); len(errs) != 0 {
		t.Fatalf("unknown fields should pass without additionalProperties:false: %+v", errs)
	}
}

func TestValidate_RootTypeMismatch(t *testing.T) {
	errs := validateJSON(t, `[1,2]`)
	if len(errs) != 1 || errs[0].Path != "" {
		t.Fatalf("errs = %+v", errs)
	}
}

func TestValidate_MalformedArgs(t *testing.T) {
	errs := validateJSON(t, `{not json`)
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "not valid JSON") {
		t.Fatalf("errs = %+v", errs)
	}
}

func TestValidate_EmptyArgsChecksRequired(t *testing.T) {
	errs := Validate(json.RawMessage(testSchema), nil)
	if len(errs) != 1 || errs[0].Path != "/clientId" {
		t.Fatalf("errs = %+v", errs)
	}
}
