package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateJSONSchema_Empty(t *testing.T) {
	schema, err := GenerateJSONSchema(nil)
	if err != nil {
		t.Fatalf("GenerateJSONSchema: %v", err)
	}

	var parsed JSONSchema
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	if parsed.Schema != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("expected $schema draft-07, got %q", parsed.Schema)
	}
	if parsed.Type != "object" {
		t.Errorf("expected type object, got %q", parsed.Type)
	}
	if parsed.AdditionalProperties {
		t.Error("expected additionalProperties false")
	}
	if len(parsed.Properties) != 0 {
		t.Errorf("expected no properties, got %d", len(parsed.Properties))
	}
}

func TestGenerateJSONSchema_AllTypes(t *testing.T) {
	options := map[string]AdapterOption{
		"verbose": {
			Type:        "boolean",
			Description: "Enable verbose logging",
			Default:     false,
		},
		"port": {
			Type:        "integer",
			Description: "Server port",
			Default:     8080,
		},
		"threshold": {
			Type:        "number",
			Description: "Detection threshold",
			Default:     0.5,
		},
		"model": {
			Type:        "string",
			Description: "Model name",
			Default:     "base",
		},
		"mode": {
			Type:        "enum",
			Description: "Operating mode",
			Values:      []any{"fast", "accurate", "balanced"},
			Default:     "balanced",
		},
	}

	schema, err := GenerateJSONSchema(options)
	if err != nil {
		t.Fatalf("GenerateJSONSchema: %v", err)
	}

	var parsed JSONSchema
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	if parsed.AdditionalProperties {
		t.Error("expected additionalProperties false")
	}

	tests := []struct {
		name     string
		propType string
		hasEnum  bool
	}{
		{"verbose", "boolean", false},
		{"port", "integer", false},
		{"threshold", "number", false},
		{"model", "string", false},
		{"mode", "string", true},
	}

	for _, tt := range tests {
		prop, exists := parsed.Properties[tt.name]
		if !exists {
			t.Errorf("property %q not found in schema", tt.name)
			continue
		}
		if prop.Type != tt.propType {
			t.Errorf("property %q: expected type %q, got %q", tt.name, tt.propType, prop.Type)
		}
		if tt.hasEnum && len(prop.Enum) == 0 {
			t.Errorf("property %q: expected enum values, got none", tt.name)
		}
	}
}

func TestGenerateJSONSchema_WithEnumConstraints(t *testing.T) {
	options := map[string]AdapterOption{
		"retries": {
			Type:        "integer",
			Description: "Retry count",
			Values:      []any{1, 3, 5},
			Default:     3,
		},
		"model": {
			Type:        "string",
			Description: "Model selection",
			Values:      []any{"tiny", "base", "small", "medium"},
			Default:     "base",
		},
	}

	schema, err := GenerateJSONSchema(options)
	if err != nil {
		t.Fatalf("GenerateJSONSchema: %v", err)
	}

	var parsed JSONSchema
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	retriesProp := parsed.Properties["retries"]
	if retriesProp.Type != "integer" {
		t.Errorf("retries: expected type integer, got %q", retriesProp.Type)
	}
	if len(retriesProp.Enum) != 3 {
		t.Errorf("retries: expected 3 enum values, got %d", len(retriesProp.Enum))
	}

	modelProp := parsed.Properties["model"]
	if modelProp.Type != "string" {
		t.Errorf("model: expected type string, got %q", modelProp.Type)
	}
	if len(modelProp.Enum) != 4 {
		t.Errorf("model: expected 4 enum values, got %d", len(modelProp.Enum))
	}
}

func TestValidateConfigAgainstOptions_EmptyOptions(t *testing.T) {
	// No options defined: config must be empty (manifest as single source of truth)
	if err := ValidateConfigAgainstOptions(nil, nil); err != nil {
		t.Errorf("empty options + empty config should pass: %v", err)
	}

	// Reject non-empty config when manifest declares no options
	config := map[string]any{"foo": "bar", "baz": 123}
	err := ValidateConfigAgainstOptions(nil, config)
	if err == nil {
		t.Fatal("expected error for non-empty config when manifest declares no options")
	}
	if !strings.Contains(err.Error(), "declares no options") {
		t.Errorf("expected 'declares no options' error, got: %v", err)
	}
}

func TestValidateConfigAgainstOptions_UnknownOption(t *testing.T) {
	options := map[string]AdapterOption{
		"model": {Type: "string"},
	}
	config := map[string]any{
		"model":   "base",
		"unknown": "value",
	}

	err := ValidateConfigAgainstOptions(options, config)
	if err == nil {
		t.Fatal("expected error for unknown option")
	}
	if !strings.Contains(err.Error(), "unknown option") || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("expected 'unknown option' error, got: %v", err)
	}
}

func TestValidateConfigAgainstOptions_TypeMismatch(t *testing.T) {
	tests := []struct {
		name    string
		optType string
		value   any
		wantErr string
	}{
		{
			name:    "boolean type mismatch",
			optType: "boolean",
			value:   "not a bool",
			wantErr: "expected boolean",
		},
		{
			name:    "integer type mismatch",
			optType: "integer",
			value:   "not an int",
			wantErr: "expected integer",
		},
		{
			name:    "number type mismatch",
			optType: "number",
			value:   "not a number",
			wantErr: "expected number",
		},
		{
			name:    "string type mismatch",
			optType: "string",
			value:   42,
			wantErr: "expected string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := map[string]AdapterOption{
				"field": {Type: tt.optType},
			}
			config := map[string]any{
				"field": tt.value,
			}

			err := ValidateConfigAgainstOptions(options, config)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateConfigAgainstOptions_EnumViolation(t *testing.T) {
	tests := []struct {
		name    string
		optType string
		values  []any
		config  any
		wantErr bool
	}{
		{
			name:    "valid enum string",
			optType: "enum",
			values:  []any{"fast", "slow"},
			config:  "fast",
			wantErr: false,
		},
		{
			name:    "invalid enum string",
			optType: "enum",
			values:  []any{"fast", "slow"},
			config:  "medium",
			wantErr: true,
		},
		{
			name:    "valid string with values",
			optType: "string",
			values:  []any{"tiny", "base", "small"},
			config:  "base",
			wantErr: false,
		},
		{
			name:    "invalid string with values",
			optType: "string",
			values:  []any{"tiny", "base", "small"},
			config:  "large",
			wantErr: true,
		},
		{
			name:    "valid integer with values",
			optType: "integer",
			values:  []any{1, 3, 5},
			config:  3,
			wantErr: false,
		},
		{
			name:    "invalid integer with values",
			optType: "integer",
			values:  []any{1, 3, 5},
			config:  7,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := map[string]AdapterOption{
				"field": {
					Type:   tt.optType,
					Values: tt.values,
				},
			}
			config := map[string]any{
				"field": tt.config,
			}

			err := ValidateConfigAgainstOptions(options, config)
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "expected one of") {
				t.Errorf("expected 'expected one of' error, got: %v", err)
			}
		})
	}
}

func TestValidateConfigAgainstOptions_ValidConfig(t *testing.T) {
	options := map[string]AdapterOption{
		"verbose": {
			Type:    "boolean",
			Default: false,
		},
		"port": {
			Type:    "integer",
			Default: 8080,
		},
		"threshold": {
			Type:    "number",
			Default: 0.5,
		},
		"model": {
			Type:    "string",
			Values:  []any{"tiny", "base", "small"},
			Default: "base",
		},
		"mode": {
			Type:    "enum",
			Values:  []any{"fast", "accurate"},
			Default: "accurate",
		},
	}

	config := map[string]any{
		"verbose":   true,
		"port":      9000,
		"threshold": 0.75,
		"model":     "small",
		"mode":      "fast",
	}

	if err := ValidateConfigAgainstOptions(options, config); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestValidateConfigAgainstOptions_NilValues(t *testing.T) {
	options := map[string]AdapterOption{
		"model": {Type: "string", Default: "base"},
		"port":  {Type: "integer", Default: 8080},
	}

	config := map[string]any{
		"model": nil,
		"port":  nil,
	}

	// nil values are allowed (means "use default")
	if err := ValidateConfigAgainstOptions(options, config); err != nil {
		t.Errorf("config with nil values rejected: %v", err)
	}
}

func TestValidateConfigAgainstOptions_TypeCoercion(t *testing.T) {
	tests := []struct {
		name    string
		optType string
		value   any
		wantErr bool
	}{
		// Valid coercions
		{"bool from string true", "boolean", "true", false},
		{"bool from string false", "boolean", "false", false},
		{"int from float", "integer", 42.0, false},
		{"int from string", "integer", "123", false},
		{"number from int", "number", 42, false},
		{"number from string", "number", "3.14", false},

		// Invalid coercions
		{"bool from invalid string", "boolean", "maybe", true},
		{"int from non-integral float", "integer", 42.5, true},
		{"int from invalid string", "integer", "abc", true},
		{"number from invalid string", "number", "xyz", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := map[string]AdapterOption{
				"field": {Type: tt.optType},
			}
			config := map[string]any{
				"field": tt.value,
			}

			err := ValidateConfigAgainstOptions(options, config)
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestGenerateJSONSchema_UnsupportedType(t *testing.T) {
	options := map[string]AdapterOption{
		"field": {Type: "unsupported"},
	}

	_, err := GenerateJSONSchema(options)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported option type") {
		t.Errorf("expected 'unsupported option type' error, got: %v", err)
	}
}

func TestValidateConfigAgainstOptions_RequiredOption(t *testing.T) {
	options := map[string]AdapterOption{
		"api_key": {
			Type:        "string",
			Description: "API key for authentication",
			Required:    true, // Required, no default
		},
		"model": {
			Type:     "string",
			Default:  "base",
			Required: false, // Optional with default
		},
	}

	t.Run("missing required option", func(t *testing.T) {
		config := map[string]any{
			"model": "small",
			// api_key is missing
		}

		err := ValidateConfigAgainstOptions(options, config)
		if err == nil {
			t.Fatal("expected error for missing required option")
		}
		if !strings.Contains(err.Error(), "missing required option") || !strings.Contains(err.Error(), "api_key") {
			t.Errorf("expected 'missing required option api_key' error, got: %v", err)
		}
	})

	t.Run("required option present", func(t *testing.T) {
		config := map[string]any{
			"api_key": "secret-key-123",
			// model is optional, can be omitted
		}

		if err := ValidateConfigAgainstOptions(options, config); err != nil {
			t.Errorf("config with required option should pass: %v", err)
		}
	})

	t.Run("all options present", func(t *testing.T) {
		config := map[string]any{
			"api_key": "secret-key-123",
			"model":   "large",
		}

		if err := ValidateConfigAgainstOptions(options, config); err != nil {
			t.Errorf("config with all options should pass: %v", err)
		}
	})
}

func TestGenerateJSONSchema_RequiredFields(t *testing.T) {
	options := map[string]AdapterOption{
		"api_key": {
			Type:        "string",
			Description: "API key",
			Required:    true,
		},
		"model": {
			Type:     "string",
			Default:  "base",
			Required: false,
		},
		"endpoint": {
			Type:        "string",
			Description: "API endpoint",
			Required:    true,
		},
	}

	schema, err := GenerateJSONSchema(options)
	if err != nil {
		t.Fatalf("GenerateJSONSchema: %v", err)
	}

	var parsed JSONSchema
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	// Should have exactly 2 required fields
	if len(parsed.Required) != 2 {
		t.Errorf("expected 2 required fields, got %d: %v", len(parsed.Required), parsed.Required)
	}

	// Check that api_key and endpoint are in Required list
	requiredMap := make(map[string]bool)
	for _, r := range parsed.Required {
		requiredMap[r] = true
	}

	if !requiredMap["api_key"] {
		t.Error("api_key should be in required list")
	}
	if !requiredMap["endpoint"] {
		t.Error("endpoint should be in required list")
	}
	if requiredMap["model"] {
		t.Error("model should NOT be in required list")
	}
}
