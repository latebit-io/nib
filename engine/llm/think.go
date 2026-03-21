package llm

import "strings"

// StripThinkTags removes <think>...</think> content from a string.
// inThink tracks state across calls for multi-line think blocks.
func StripThinkTags(s string, inThink *bool) string {
	var out strings.Builder
	for len(s) > 0 {
		if *inThink {
			end := strings.Index(s, "</think>")
			if end == -1 {
				return out.String()
			}
			s = s[end+len("</think>"):]
			*inThink = false
		} else {
			start := strings.Index(s, "<think>")
			if start == -1 {
				out.WriteString(s)
				return out.String()
			}
			out.WriteString(s[:start])
			s = s[start+len("<think>"):]
			*inThink = true
		}
	}
	return out.String()
}
