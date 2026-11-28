package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/latebit-io/nib/kit/elicit"
	"github.com/latebit-io/nib/kit/internal/httporigin"
)

// handleElicit answers a server's question through the asker. The SDK
// has already checked the schema, the mode and, later, the answer.
func (c *Client) handleElicit(ctx context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
	cancel := &sdk.ElicitResult{Action: elicit.Cancel}
	r, err := request(req.Params)
	if err != nil {
		slog.Warn("mcp: a question from the server cannot be shown", "err", err)
		return cancel, nil
	}
	c.mu.Lock()
	ask := c.ask
	c.mu.Unlock()
	if ask == nil {
		return cancel, nil
	}
	res, err := ask(ctx, r)
	if err != nil {
		return nil, err
	}
	return &sdk.ElicitResult{Action: res.Action, Content: res.Content}, nil
}

// SetAsker sets who answers a server's questions to the person.
// Without one, every question is answered as dismissed.
func (c *Client) SetAsker(a elicit.Asker) {
	c.mu.Lock()
	c.ask = a
	c.mu.Unlock()
}

// request converts the SDK's question into what a frontend shows.
func request(p *sdk.ElicitParams) (elicit.Request, error) {
	r := elicit.Request{Mode: elicit.Form, Message: p.Message}
	if p.Mode == elicit.Page {
		// Shown as a page to open, so only a web address will do.
		if _, err := httporigin.Page(p.URL); err != nil {
			return elicit.Request{}, err
		}
		r.Mode, r.URL = elicit.Page, p.URL
		return r, nil
	}
	schema, err := json.Marshal(p.RequestedSchema)
	if err != nil {
		return elicit.Request{}, fmt.Errorf("schema: %w", err)
	}
	r.Fields, err = elicit.ParseFields(schema)
	return r, err
}
