// Package schema implements the subset of JSON Schema the tool input
// schemas in internal/mcp/tools use, so tools/call arguments can be
// validated in one shared path before any handler runs (Section 12.6,
// Section 13 "Invalid params"). Supported keywords: type, properties,
// required, minimum, maximum, enum, items, additionalProperties. Unknown
// keywords are ignored, matching JSON Schema's permissive semantics.
package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidationError describes one failed constraint, addressed by a
// JSON-Pointer-style path into the arguments ("" is the document root).
type ValidationError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type node struct {
	Type                 string          `json:"type"`
	Properties           map[string]node `json:"properties"`
	Required             []string        `json:"required"`
	Minimum              *float64        `json:"minimum"`
	Maximum              *float64        `json:"maximum"`
	Enum                 []any           `json:"enum"`
	Items                *node           `json:"items"`
	AdditionalProperties json.RawMessage `json:"additionalProperties"`
}

// Validate checks args against schema and returns every violated
// constraint (nil means valid). A schema or argument document that is not
// itself valid JSON yields a single root-level error.
func Validate(schema, args json.RawMessage) []ValidationError {
	var s node
	if err := json.Unmarshal(schema, &s); err != nil {
		return []ValidationError{{Path: "", Message: "invalid schema: " + err.Error()}}
	}
	var v any
	if len(args) == 0 {
		v = map[string]any{}
	} else if err := json.Unmarshal(args, &v); err != nil {
		return []ValidationError{{Path: "", Message: "arguments are not valid JSON"}}
	}
	return validate(&s, v, "")
}

func validate(s *node, v any, path string) []ValidationError {
	var errs []ValidationError

	if s.Type != "" && !typeMatches(s.Type, v) {
		return []ValidationError{{Path: path, Message: fmt.Sprintf("expected %s, got %s", s.Type, typeName(v))}}
	}

	if len(s.Enum) > 0 && !enumContains(s.Enum, v) {
		errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf("must be one of %s", enumList(s.Enum))})
	}

	if n, ok := asNumber(v); ok {
		if s.Minimum != nil && n < *s.Minimum {
			errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf("must be >= %v", *s.Minimum)})
		}
		if s.Maximum != nil && n > *s.Maximum {
			errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf("must be <= %v", *s.Maximum)})
		}
	}

	if obj, ok := v.(map[string]any); ok {
		for _, req := range s.Required {
			if val, present := obj[req]; !present || val == nil {
				errs = append(errs, ValidationError{Path: join(path, req), Message: "required field is missing"})
			}
		}
		for key, val := range obj {
			if prop, ok := s.Properties[key]; ok {
				errs = append(errs, validate(&prop, val, join(path, key))...)
				continue
			}
			if len(s.AdditionalProperties) > 0 {
				errs = append(errs, validateAdditional(s.AdditionalProperties, val, join(path, key))...)
			}
		}
	}

	if arr, ok := v.([]any); ok && s.Items != nil {
		for i, item := range arr {
			errs = append(errs, validate(s.Items, item, fmt.Sprintf("%s/%d", path, i))...)
		}
	}

	return errs
}

// validateAdditional handles additionalProperties, which may be a boolean
// (false forbids unknown keys) or a schema applied to each unknown key.
func validateAdditional(raw json.RawMessage, v any, path string) []ValidationError {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if !b {
			return []ValidationError{{Path: path, Message: "unknown field"}}
		}
		return nil
	}
	var sub node
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil
	}
	return validate(&sub, v, path)
}

func typeMatches(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := asNumber(v)
		return ok
	case "integer":
		n, ok := asNumber(v)
		return ok && n == float64(int64(n))
	case "null":
		return v == nil
	default:
		return true
	}
}

func typeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, json.Number:
		return "number"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func enumContains(enum []any, v any) bool {
	for _, e := range enum {
		if e == v {
			return true
		}
	}
	return false
}

func enumList(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		parts = append(parts, fmt.Sprintf("%v", e))
	}
	return strings.Join(parts, ", ")
}

func join(path, key string) string {
	return path + "/" + key
}
