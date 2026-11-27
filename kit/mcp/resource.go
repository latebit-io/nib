package mcp

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// The tools that reach every server's resources. Two tools for all
// servers, not two per server: resources are many and change often.
const (
	ListResourcesTool = "list_mcp_resources"
	ReadResourceTool  = "read_mcp_resource"
)

// maxListed caps the resources one listing names per server.
const maxListed = 200

// listing is one resource or resource template, as a listing shows it.
type listing struct {
	address, label, description, mime string
}

// line renders the listing for the model.
func (l listing) line() string {
	var parts []string
	if l.description != "" {
		parts = append(parts, l.description)
	}
	if l.mime != "" {
		parts = append(parts, "("+l.mime+")")
	}
	text := "- " + l.address + "  " + l.label
	if len(parts) > 0 {
		text += ": " + strings.Join(parts, " ")
	}
	return text + "\n"
}

// resources lists up to limit of the server's resources.
func (c *Client) resources(ctx context.Context, limit int) ([]listing, error) {
	return collect(ctx, c, (*sdk.ClientSession).Resources, limit, func(r *sdk.Resource) (listing, error) {
		return listing{address: r.URI, label: cmp.Or(r.Title, r.Name), description: r.Description, mime: r.MIMEType}, nil
	})
}

// templates lists the server's resource templates.
func (c *Client) templates(ctx context.Context) ([]listing, error) {
	return collect(ctx, c, (*sdk.ClientSession).ResourceTemplates, 0, func(r *sdk.ResourceTemplate) (listing, error) {
		return listing{address: "template " + r.URITemplate, label: cmp.Or(r.Title, r.Name), description: r.Description, mime: r.MIMEType}, nil
	})
}

// readResource reads a resource and returns its contents as text;
// binary contents are named, not included.
func (c *Client) readResource(ctx context.Context, uri string) (string, error) {
	session, err := c.active()
	if err != nil {
		return "", err
	}
	res, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		return "", wrap(err)
	}
	parts := make([]string, 0, len(res.Contents))
	for _, content := range res.Contents {
		if content != nil {
			parts = append(parts, renderResource(content))
		}
	}
	return strings.Join(parts, "\n"), nil
}

// resourceArgs are both tools' arguments.
type resourceArgs struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

// resourceTools returns the two resource tools for the servers that
// offer resources, or none.
func resourceTools(s *Session, servers []string) []agent.Tool {
	if len(servers) == 0 {
		return nil
	}
	names := make([]any, len(servers))
	for i, name := range servers {
		names[i] = name
	}
	list := llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        ListResourcesTool,
		Description: "List the resources (documents and data) the MCP servers offer, with the URI to read each by. Read one with " + ReadResourceTool + ".",
		Parameters: llm.FunctionParams{Type: "object", Properties: map[string]llm.FunctionParam{
			"server": {Type: "string", Description: "Only this server. Leave it out for every server.", Enum: names},
		}},
	}}
	read := llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        ReadResourceTool,
		Description: "Read a resource from an MCP server by its URI, as " + ListResourcesTool + " lists it or a resource template builds it.",
		Parameters: llm.FunctionParams{Type: "object", Required: []string{"server", "uri"}, Properties: map[string]llm.FunctionParam{
			"server": {Type: "string", Description: "The server's name.", Enum: names},
			"uri":    {Type: "string", Description: "The resource's URI."},
		}},
	}}
	return []agent.Tool{&resourceTool{def: list, run: s.listResources}, &resourceTool{def: read, run: s.readResource}}
}

// resourceTool is one of the two resource tools.
type resourceTool struct {
	def llm.ToolDef
	run func(ctx context.Context, args resourceArgs) (string, error)
}

// Definition implements [agent.Tool].
func (t *resourceTool) Definition() llm.ToolDef { return t.def }

// Execute implements [agent.Tool].
func (t *resourceTool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	args, err := llm.DecodeArguments[resourceArgs](t.def.Function.Parameters, call.Function.Arguments)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("Error: invalid arguments: %v", err), IsError: true}
	}
	text, err := t.run(ctx, args)
	if err != nil {
		return agent.ToolResult{Content: "Error: " + err.Error(), IsError: true}
	}
	return agent.ToolResult{Content: capText(text)}
}

// listResources renders the resources of one server, or of every
// server that has them, all servers at once.
func (s *Session) listResources(ctx context.Context, args resourceArgs) (string, error) {
	targets := slices.DeleteFunc(s.Servers(), func(c Connected) bool {
		return !c.resources || (args.Server != "" && c.Server.Name != args.Server)
	})
	if len(targets) == 0 {
		return "", fmt.Errorf("no server named %q offers resources", args.Server)
	}
	parts := make([]string, len(targets))
	var wg sync.WaitGroup
	for i, c := range targets {
		wg.Go(func() { parts[i] = renderListing(ctx, c) })
	}
	wg.Wait()
	return strings.Join(parts, ""), nil
}

// renderListing lists one server's resources and templates, both at once.
func renderListing(ctx context.Context, c Connected) string {
	var resources, templates []listing
	var err error
	var wg sync.WaitGroup
	wg.Go(func() { resources, err = c.client.resources(ctx, maxListed+1) })
	// Templates are optional; a server without them answers with an error.
	wg.Go(func() { templates, _ = c.client.templates(ctx) })
	wg.Wait()
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", c.Server.Name)
	if err != nil {
		fmt.Fprintf(&b, "(listing failed: %v)\n", err)
	}
	for i, r := range resources {
		if i == maxListed {
			b.WriteString("(more not listed)\n")
			break
		}
		b.WriteString(r.line())
	}
	for _, t := range templates {
		b.WriteString(t.line())
	}
	if err == nil && len(resources) == 0 && len(templates) == 0 {
		b.WriteString("(no resources)\n")
	}
	return b.String()
}

// readResource reads one resource of a server.
func (s *Session) readResource(ctx context.Context, args resourceArgs) (string, error) {
	servers := s.Servers()
	i := slices.IndexFunc(servers, func(c Connected) bool { return c.Server.Name == args.Server && c.resources })
	if i < 0 {
		return "", fmt.Errorf("no server named %q offers resources", args.Server)
	}
	return servers[i].client.readResource(ctx, args.URI)
}
