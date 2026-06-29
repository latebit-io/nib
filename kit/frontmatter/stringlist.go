package frontmatter

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// StringList is a frontmatter field that accepts either a single scalar
// string or a sequence of strings, normalizing both to a []string. It
// exists because the Claude Code frontmatter conventions nib imports
// (e.g. allowed-tools / disallowed-tools) permit either form:
//
//	allowed-tools: Bash(git *) Read      # scalar → ["Bash(git *) Read"]
//	allowed-tools:                        # sequence → ["Bash(git *)", "Read"]
//	  - Bash(git *)
//	  - Read
//
// A scalar is kept as a single element (callers that need per-rule tokens
// split it downstream); a sequence decodes element-wise. An empty/absent
// field yields a nil slice.
type StringList []string

// UnmarshalYAML implements [yaml.Unmarshaler], accepting a scalar or a
// sequence node.
func (l *StringList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		if s == "" {
			*l = nil
			return nil
		}
		*l = StringList{s}
		return nil
	case yaml.SequenceNode:
		var ss []string
		if err := value.Decode(&ss); err != nil {
			return err
		}
		*l = ss
		return nil
	default:
		return fmt.Errorf("frontmatter: expected string or list, got yaml kind %d", value.Kind)
	}
}
