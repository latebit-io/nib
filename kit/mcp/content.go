package mcp

import (
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// renderContent turns content blocks into text for the model. Binary
// content is named, not included: the model reads text.
func renderContent(blocks []sdk.Content) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if text := renderBlock(block); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// renderBlock renders one content block.
func renderBlock(block sdk.Content) string {
	switch b := block.(type) {
	case *sdk.TextContent:
		return b.Text
	case *sdk.ImageContent:
		return fmt.Sprintf("[image, %s, %d bytes, not shown]", b.MIMEType, len(b.Data))
	case *sdk.AudioContent:
		return fmt.Sprintf("[audio, %s, %d bytes, not shown]", b.MIMEType, len(b.Data))
	case *sdk.ResourceLink:
		return fmt.Sprintf("[resource %s: %s]", b.Name, b.URI)
	case *sdk.EmbeddedResource:
		if b.Resource == nil {
			return ""
		}
		return renderResource(b.Resource)
	}
	return ""
}

// renderResource renders a resource's contents: its text, or a line
// naming binary data.
func renderResource(r *sdk.ResourceContents) string {
	if r.Text != "" || len(r.Blob) == 0 {
		return r.Text
	}
	return fmt.Sprintf("[binary resource %s, %s, %d bytes, not shown]", r.URI, r.MIMEType, len(r.Blob))
}
