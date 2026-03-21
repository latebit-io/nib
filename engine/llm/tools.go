package llm

// toolDef is the OpenAI-compatible tool definition sent in API requests.
type toolDef struct {
	Type     string      `json:"type"` // "function"
	Function functionDef `json:"function"`
}

// functionDef describes a function the LLM can call.
type functionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  functionParams `json:"parameters"`
}

// functionParams is a JSON Schema object for function parameters.
type functionParams struct {
	Type       string                   `json:"type"` // "object"
	Properties map[string]functionParam `json:"properties"`
	Required   []string                 `json:"required"`
}

// functionParam describes one parameter.
type functionParam struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// editFileTool is the tool definition for search-and-replace editing.
var editFileTool = toolDef{
	Type: "function",
	Function: functionDef{
		Name:        "edit_file",
		Description: "Search for exact text in the file and replace it with new text. The search string must match the file content exactly (including whitespace and newlines). To delete text, set replace to an empty string. To insert, include anchor text in search and repeat it in replace with the new code added.",
		Parameters: functionParams{
			Type: "object",
			Properties: map[string]functionParam{
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

// readFileTool lets the LLM read the current file content before editing.
var readFileTool = toolDef{
	Type: "function",
	Function: functionDef{
		Name:        "read_file",
		Description: "Read the current contents of the file being edited. Use this before making an edit to see the latest state of the file, especially after the developer may have made changes.",
		Parameters: functionParams{
			Type:       "object",
			Properties: map[string]functionParam{},
			Required:   []string{},
		},
	},
}

// defaultTools is the set of tools provided to the LLM.
var defaultTools = []toolDef{readFileTool, editFileTool}
