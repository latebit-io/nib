package llm

// ToolDef is the OpenAI-compatible tool definition sent in API requests.
type ToolDef struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a function the LLM can call.
type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  FunctionParams `json:"parameters"`
}

// FunctionParams is a JSON Schema object for function parameters.
type FunctionParams struct {
	Type       string                   `json:"type"` // "object"
	Properties map[string]FunctionParam `json:"properties"`
	Required   []string                 `json:"required"`
}

// FunctionParam describes one parameter.
type FunctionParam struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}
