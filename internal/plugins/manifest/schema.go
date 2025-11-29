package manifest

import (
	"encoding/json"
	"fmt"
)

// JSONSchema represents a JSON Schema document for adapter configuration.
type JSONSchema struct {
	Schema               string                       `json:"$schema"`
	Type                 string                       `json:"type"`
	Properties           map[string]JSONSchemaProperty `json:"properties,omitempty"`
	Required             []string                     `json:"required,omitempty"`
	AdditionalProperties bool                         `json:"additionalProperties"`
}

// JSONSchemaProperty represents a property in JSON Schema.
type JSONSchemaProperty struct {
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Default     any      `json:"default,omitempty"`
	Enum        []any    `json:"enum,omitempty"`
	OneOf       []any    `json:"oneOf,omitempty"`
}

// GenerateJSONSchema creates a JSON Schema from adapter option definitions.
// The schema validates adapter config JSON against manifest-declared options.
func GenerateJSONSchema(options map[string]AdapterOption) ([]byte, error) {
	if len(options) == 0 {
		// Empty config is valid
		schema := JSONSchema{
			Schema:               "http://json-schema.org/draft-07/schema#",
			Type:                 "object",
			AdditionalProperties: false,
		}
		return json.MarshalIndent(schema, "", "  ")
	}

	schema := JSONSchema{
		Schema:               "http://json-schema.org/draft-07/schema#",
		Type:                 "object",
		Properties:           make(map[string]JSONSchemaProperty),
		AdditionalProperties: false,
	}

	for key, opt := range options {
		prop := JSONSchemaProperty{
			Description: opt.Description,
			Default:     opt.Default,
		}

		// Map NAP option type to JSON Schema type
		switch opt.Type {
		case "boolean":
			prop.Type = "boolean"
		case "integer":
			prop.Type = "integer"
		case "number":
			prop.Type = "number"
		case "string":
			prop.Type = "string"
		case "enum":
			prop.Type = "string"
			if len(opt.Values) > 0 {
				prop.Enum = opt.Values
			}
		default:
			return nil, fmt.Errorf("unsupported option type %q for option %q", opt.Type, key)
		}

		// Add enum constraint for non-enum types that have Values
		if opt.Type != "enum" && len(opt.Values) > 0 {
			prop.Enum = opt.Values
		}

		schema.Properties[key] = prop

		// Track required options
		if opt.Required {
			schema.Required = append(schema.Required, key)
		}
	}

	return json.MarshalIndent(schema, "", "  ")
}

// ValidateConfigAgainstOptions validates adapter config against manifest options.
// This is a lightweight validation that checks:
// - No unknown options (not declared in manifest)
// - All required options are present in config
// - All values match declared types
// - Enum/value constraints are satisfied
//
// Manifest is the single source of truth: if no options are declared,
// config MUST be empty or nil.
//
// Required vs Optional:
//   - Options with Required=true MUST be present in config
//   - Options with Required=false (default) are optional and use Default if not provided
//
// Returns detailed error if validation fails.
func ValidateConfigAgainstOptions(options map[string]AdapterOption, config map[string]any) error {
	// If manifest declares no options, config must be empty
	if len(options) == 0 {
		if len(config) > 0 {
			return fmt.Errorf("adapter manifest declares no options, but config contains %d field(s)", len(config))
		}
		return nil
	}

	// Check for unknown options
	for key := range config {
		if _, known := options[key]; !known {
			return fmt.Errorf("unknown option %q (not declared in manifest)", key)
		}
	}

	// Check for missing required options
	for key, opt := range options {
		if opt.Required {
			if _, exists := config[key]; !exists {
				return fmt.Errorf("missing required option %q", key)
			}
		}
	}

	// Validate each config value against its option definition
	for key, value := range config {
		if value == nil {
			continue
		}

		opt, exists := options[key]
		if !exists {
			return fmt.Errorf("unknown option %q", key)
		}

		if err := validateOptionValue(opt, key, value); err != nil {
			return err
		}
	}

	return nil
}

func validateOptionValue(opt AdapterOption, key string, value any) error {
	switch opt.Type {
	case "boolean":
		if _, err := coerceBool(value); err != nil {
			return fmt.Errorf("option %q: %w", key, err)
		}
		// Check enum constraint if present
		if len(opt.Values) > 0 {
			boolVal, _ := coerceBool(value)
			if !containsValue(opt.Values, boolVal) {
				return fmt.Errorf("option %q: expected one of %v, got %v", key, opt.Values, boolVal)
			}
		}

	case "integer":
		if _, err := coerceInt(value); err != nil {
			return fmt.Errorf("option %q: %w", key, err)
		}
		// Check enum constraint if present
		if len(opt.Values) > 0 {
			intVal, _ := coerceInt(value)
			if !containsValue(opt.Values, intVal) {
				return fmt.Errorf("option %q: expected one of %v, got %v", key, opt.Values, intVal)
			}
		}

	case "number":
		if _, err := coerceNumber(value); err != nil {
			return fmt.Errorf("option %q: %w", key, err)
		}
		// Check enum constraint if present
		if len(opt.Values) > 0 {
			numVal, _ := coerceNumber(value)
			if !containsValue(opt.Values, numVal) {
				return fmt.Errorf("option %q: expected one of %v, got %v", key, opt.Values, numVal)
			}
		}

	case "string":
		if _, err := coerceString(value); err != nil {
			return fmt.Errorf("option %q: %w", key, err)
		}
		// Check enum constraint if present
		if len(opt.Values) > 0 {
			strVal, _ := coerceString(value)
			if !containsValue(opt.Values, strVal) {
				return fmt.Errorf("option %q: expected one of %v, got %q", key, opt.Values, strVal)
			}
		}

	case "enum":
		strVal, err := coerceString(value)
		if err != nil {
			return fmt.Errorf("option %q: %w", key, err)
		}
		if !containsValue(opt.Values, strVal) {
			return fmt.Errorf("option %q: expected one of %v, got %q", key, opt.Values, strVal)
		}

	default:
		return fmt.Errorf("option %q: unsupported type %q", key, opt.Type)
	}

	return nil
}
