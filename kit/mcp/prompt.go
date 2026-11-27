package mcp

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/latebit-io/nib/kit/command"
)

// promptInfo is a prompt template a server offers, for the person to use
// as a slash command.
type promptInfo struct {
	name, description string
	arguments         []promptArgument
}

// promptArgument is one value a prompt takes.
type promptArgument struct {
	name     string
	required bool
}

// prompts lists the server's prompts.
func (c *Client) prompts(ctx context.Context) ([]promptInfo, error) {
	return collect(ctx, c, (*sdk.ClientSession).Prompts, 0, func(p *sdk.Prompt) (promptInfo, error) {
		info := promptInfo{name: p.Name, description: cmp.Or(p.Description, p.Title)}
		for _, a := range p.Arguments {
			if a != nil {
				info.arguments = append(info.arguments, promptArgument{name: a.Name, required: a.Required})
			}
		}
		return info, nil
	})
}

// getPrompt fills a prompt with args and returns it as one message's
// text. A turn the prompt gives the assistant is marked as such.
func (c *Client) getPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	session, err := c.active()
	if err != nil {
		return "", err
	}
	res, err := session.GetPrompt(ctx, &sdk.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		return "", wrap(err)
	}
	parts := make([]string, 0, len(res.Messages))
	for _, m := range res.Messages {
		if m == nil {
			continue
		}
		text := renderBlock(m.Content)
		if text == "" {
			continue
		}
		if m.Role == "assistant" {
			text = "Assistant: " + text
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n"), nil
}

// promptCommand is a server's prompt as a slash command: the filled
// prompt is sent as the person's message.
type promptCommand struct {
	client *Client
	server string
	prompt promptInfo
}

// Definition implements [command.Command].
func (p *promptCommand) Definition() command.Definition {
	desc := p.prompt.description
	if usage := p.usage(); usage != "" {
		desc = strings.TrimSpace(desc + " Usage: " + usage)
	}
	return command.Definition{
		Name: ToolName(p.server, p.prompt.name), Description: desc,
		Source: command.Source{Kind: command.SourceMCP, Path: "mcp:" + p.server},
	}
}

// usage names the arguments, optional ones in brackets.
func (p *promptCommand) usage() string {
	names := make([]string, 0, len(p.prompt.arguments))
	for _, a := range p.prompt.arguments {
		if a.required {
			names = append(names, "<"+a.name+">")
		} else {
			names = append(names, "["+a.name+"]")
		}
	}
	return strings.Join(names, " ")
}

// Handle implements [command.HandlerCommand]. The server may ask the
// person something before it answers, so where the frontend can take
// the result later, the prompt is fetched off its loop.
func (p *promptCommand) Handle(ctx context.Context, sess command.Session, args string) error {
	values, err := p.values(args)
	if err != nil {
		return err
	}
	poster, ok := sess.(command.Poster)
	if !ok {
		text, err := p.fetch(ctx, values)
		if err != nil {
			return err
		}
		return sess.SubmitPrompt(ctx, text)
	}
	sess.Display("(getting the prompt from " + p.server + ")")
	go func() {
		text, err := p.fetch(ctx, values)
		poster.Post(func() {
			if err == nil {
				err = sess.SubmitPrompt(ctx, text)
			}
			if err != nil {
				sess.Display("error: /" + p.Definition().Name + ": " + err.Error())
			}
		})
	}()
	return nil
}

// fetch fills the prompt on the server.
func (p *promptCommand) fetch(ctx context.Context, values map[string]string) (string, error) {
	text, err := p.client.getPrompt(ctx, p.prompt.name, values)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("the prompt %s came back empty", p.prompt.name)
	}
	return text, nil
}

// values assigns the typed words to the arguments in order; the last
// argument takes the rest of the line, spaces and all.
func (p *promptCommand) values(line string) (map[string]string, error) {
	values := map[string]string{}
	rest := strings.TrimSpace(line)
	for i, a := range p.prompt.arguments {
		if rest == "" {
			if a.required {
				return nil, fmt.Errorf("%s is required; usage: /%s %s", a.name, p.Definition().Name, p.usage())
			}
			continue
		}
		if i == len(p.prompt.arguments)-1 {
			values[a.name] = rest
			break
		}
		word, after, _ := strings.Cut(rest, " ")
		values[a.name] = word
		rest = strings.TrimSpace(after)
	}
	return values, nil
}
