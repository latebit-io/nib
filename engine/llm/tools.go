package llm

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

// FunctionParams is a JSON Schema object for function parameters.
type FunctionParams struct {
	// Type is the JSON Schema type (always "object").
	Type string `json:"type"`
	// Properties maps parameter names to their schemas.
	Properties map[string]FunctionParam `json:"properties"`
	// Required lists the parameter names that must be provided.
	Required []string `json:"required"`
}

// FunctionParam describes one parameter.
type FunctionParam struct {
	// Type is the JSON Schema type (e.g., "string", "integer").
	Type string `json:"type"`
	// Description explains the parameter's purpose (shown to the LLM).
	Description string `json:"description"`
}
