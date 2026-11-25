package llm

import (
	"encoding/json"
	"fmt"
	"slices"
)

// ToolDef is the OpenAI-compatible tool definition sent in API requests.
type ToolDef struct {
	// Type is the tool type (always "function").
	Type string `json:"type"`
	// Function is the function definition.
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a function the LLM can call.
type FunctionDef struct {
	// Name is the function identifier used in tool calls.
	Name string `json:"name"`
	// Description explains what the function does (shown to the LLM).
	Description string `json:"description"`
	// Parameters is the JSON Schema for the function's input.
	Parameters FunctionParams `json:"parameters"`
}

// FunctionParams is a JSON Schema object for function parameters. It is
// sent as written to every provider: no keyword is stripped or adapted, so
// an endpoint that lacks one rejects the request instead of losing it.
type FunctionParams struct {
	// Type is the JSON Schema type (always "object").
	Type string `json:"type"`
	// Properties maps parameter names to their schemas.
	Properties map[string]FunctionParam `json:"properties"`
	// Required lists the parameter names that must be provided.
	Required []string `json:"required"`
	// AdditionalProperties, when false, forbids parameters not in Properties.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`
}

// MarshalJSON writes nil Properties and Required as {} and []: strict
// validators reject null there.
func (p FunctionParams) MarshalJSON() ([]byte, error) {
	type plain FunctionParams
	if p.Properties == nil {
		p.Properties = map[string]FunctionParam{}
	}
	if p.Required == nil {
		p.Required = []string{}
	}
	return json.Marshal(plain(p))
}

// object is the parameters as the object schema they are.
func (p FunctionParams) object() FunctionParam {
	return FunctionParam{
		Type: "object", Properties: p.Properties, Required: p.Required,
		AdditionalProperties: p.AdditionalProperties,
	}
}

// params is an object schema as a tool's top-level parameters.
func (p FunctionParam) params() FunctionParams {
	return FunctionParams{
		Type: p.Type, Properties: p.Properties, Required: p.Required,
		AdditionalProperties: p.AdditionalProperties,
	}
}

// FunctionParam describes one parameter as a JSON Schema fragment. Only
// Type and Description are always written; the rest are omitted when empty.
// Strict validators require Items whenever Type is "array".
type FunctionParam struct {
	// Type is the JSON Schema type (e.g., "string", "integer", "array", "object").
	Type string `json:"type"`
	// Description explains the parameter's purpose (shown to the LLM).
	Description string `json:"description"`
	// Items is the schema for array elements. Required when Type is "array".
	Items *FunctionParam `json:"items,omitempty"`
	// Properties maps field names to schemas for object-typed parameters.
	Properties map[string]FunctionParam `json:"properties,omitempty"`
	// Required lists the required field names for object-typed parameters.
	Required []string `json:"required,omitempty"`
	// Nullable also admits null: Type is written as ["T","null"] and null
	// joins a non-empty Enum.
	Nullable bool `json:"-"`
	// Enum, when non-empty, lists the only values allowed.
	Enum []any `json:"enum,omitempty"`
	// Minimum is the inclusive lower bound of a numeric parameter.
	Minimum *float64 `json:"minimum,omitempty"`
	// Maximum is the inclusive upper bound of a numeric parameter.
	Maximum *float64 `json:"maximum,omitempty"`
	// AdditionalProperties, when false, forbids fields not in Properties.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`
}

// MarshalJSON writes a Nullable parameter as a type union with null.
func (p FunctionParam) MarshalJSON() ([]byte, error) {
	type plain FunctionParam
	if !p.Nullable {
		return json.Marshal(plain(p))
	}
	if len(p.Enum) > 0 && !slices.Contains(p.Enum, nil) {
		p.Enum = append(slices.Clone(p.Enum), nil)
	}
	// The outer Type shadows the embedded one.
	return json.Marshal(struct {
		Type [2]string `json:"type"`
		plain
	}{[2]string{p.Type, "null"}, plain(p)})
}

// UnmarshalJSON reads what MarshalJSON writes. A union of two non-null
// types has no Go form here and is an error.
func (p *FunctionParam) UnmarshalJSON(data []byte) error {
	type plain FunctionParam
	aux := struct {
		Type json.RawMessage `json:"type"`
		*plain
	}{plain: (*plain)(p)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Type) == 0 || json.Unmarshal(aux.Type, &p.Type) == nil {
		return nil
	}
	var union []string
	if err := json.Unmarshal(aux.Type, &union); err != nil {
		return fmt.Errorf("schema type %s: want a string or a list of strings", aux.Type)
	}
	p.Nullable = slices.Contains(union, "null")
	union = slices.DeleteFunc(union, func(t string) bool { return t == "null" })
	if len(union) != 1 {
		return fmt.Errorf("schema type %s: only a union of one type with null is supported", aux.Type)
	}
	p.Type = union[0]
	if p.Nullable {
		p.Enum = slices.DeleteFunc(p.Enum, func(v any) bool { return v == nil })
	}
	return nil
}
