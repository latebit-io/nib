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

// EditFileTool is the tool definition for search-and-replace editing.
var EditFileTool = ToolDef{
	Type: "function",
	Function: FunctionDef{
		Name:        "edit_file",
		Description: "Search for exact text in the file and replace it with new text. The search string must match the file content exactly (including whitespace and newlines). To delete text, set replace to an empty string. To insert, include anchor text in search and repeat it in replace with the new code added.",
		Parameters: FunctionParams{
			Type: "object",
			Properties: map[string]FunctionParam{
				"search": {
					Type:        "string",
					Description: "Exact text to find in the file. Must match verbatim.",
				},
				"replace": {
					Type:        "string",
					Description: "Text to replace the search text with. Empty string to delete.",
				},
				"reason": {
					Type:        "string",
					Description: "Brief explanation of the change, shown to the developer.",
				},
			},
			Required: []string{"search", "replace", "reason"},
		},
	},
}

// DefaultTools is the set of tools provided to the LLM.
var DefaultTools = []ToolDef{EditFileTool}
