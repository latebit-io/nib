package a2a

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// Polling a task that is still running when the call returns.
const (
	firstPoll = 500 * time.Millisecond
	maxPoll   = 5 * time.Second
)

// Text from a card is cut to these lengths in a tool's description.
const (
	maxDescription      = 600
	maxSkillDescription = 200
	maxSkills           = 10
)

// remote is one connected agent and its tool. It keeps no conversation:
// each answer names its own, so callers that share the tool stay apart.
type remote struct {
	agent  Agent
	client *a2aclient.Client
	def    llm.ToolDef
}

// newRemote returns the tool for a's agent, described from its card.
func newRemote(a Agent, card *a2a.AgentCard, c *a2aclient.Client) *remote {
	var desc strings.Builder
	fmt.Fprintf(&desc, "Sends a message to %s, a remote agent, over A2A and returns its answer. ", clean(card.Name, 80))
	if d := clean(card.Description, maxDescription); d != "" {
		fmt.Fprintf(&desc, "It describes itself: %s ", d)
	}
	for i, s := range card.Skills {
		if i == maxSkills {
			fmt.Fprintf(&desc, "(%d more skills) ", len(card.Skills)-maxSkills)
			break
		}
		fmt.Fprintf(&desc, "Skill %s: %s ", clean(s.Name, 80), clean(s.Description, maxSkillDescription))
	}
	desc.WriteString("The message leaves this machine. To go on with a conversation, pass the conversation (and task) an earlier answer named.")
	params := llm.FunctionParams{Type: "object", Required: []string{"message"}, Properties: map[string]llm.FunctionParam{
		"message":      {Type: "string", Description: "What to ask or tell the agent, with everything it needs: it sees nothing of this conversation."},
		"conversation": {Type: "string", Description: "The conversation an earlier answer named, to continue it. Omit to start a new one."},
		"task":         {Type: "string", Description: "The task an earlier answer said waits for input, when this message answers it."},
	}}
	return &remote{agent: a, client: c, def: llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name: ToolNamePrefix + a.Name, Description: desc.String(), Parameters: params,
	}}}
}

// Definition implements [agent.Tool].
func (r *remote) Definition() llm.ToolDef { return r.def }

// Execute implements [agent.Tool]: it sends the message and waits for
// the agent's answer, or for its question when it needs more.
func (r *remote) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	args, err := llm.DecodeArguments[struct {
		Message      string `json:"message"`
		Conversation string `json:"conversation"`
		Task         string `json:"task"`
	}](r.def.Function.Parameters, call.Function.Arguments)
	if err != nil {
		return failed("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Message) == "" {
		return failed("message is empty")
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(args.Message))
	msg.ContextID, msg.TaskID = args.Conversation, a2a.TaskID(args.Task)
	res, err := r.client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		return failed("%s: %v", r.agent.Name, err)
	}
	switch v := res.(type) {
	case *a2a.Message:
		return agent.ToolResult{Content: text(v.Parts) + footer(v.ContextID, "")}
	case *a2a.Task:
		task, err := r.finish(ctx, v)
		if err != nil {
			return failed("%s: %v", r.agent.Name, err)
		}
		return r.answer(task)
	}
	return failed("%s answered with %T, which nib does not read", r.agent.Name, res)
}

// finish polls task until it ends or asks for something.
func (r *remote) finish(ctx context.Context, task *a2a.Task) (*a2a.Task, error) {
	wait := firstPoll
	none := 0 // the answer is in the status and artifacts, not the history
	for !task.Status.State.Terminal() && !paused(task.Status.State) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, maxPoll)
		next, err := r.client.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID, HistoryLength: &none})
		if err != nil {
			return nil, err
		}
		task = next
	}
	return task, nil
}

// paused reports whether a task waits for the caller.
func paused(s a2a.TaskState) bool {
	return s == a2a.TaskStateInputRequired || s == a2a.TaskStateAuthRequired
}

// answer is the tool result for a task that ended or paused.
func (r *remote) answer(task *a2a.Task) agent.ToolResult {
	var status string
	if task.Status.Message != nil {
		status = text(task.Status.Message.Parts)
	}
	switch task.Status.State {
	case a2a.TaskStateCompleted:
		var parts []string
		for _, a := range task.Artifacts {
			if a != nil {
				parts = append(parts, text(a.Parts))
			}
		}
		out := strings.TrimSpace(strings.Join(append(parts, status), "\n\n"))
		if out == "" {
			out = "(the agent finished without an answer)"
		}
		return agent.ToolResult{Content: out + footer(task.ContextID, "")}
	case a2a.TaskStateInputRequired:
		return agent.ToolResult{Content: status + footer(task.ContextID, task.ID)}
	case a2a.TaskStateAuthRequired:
		return failed("%s needs a sign-in nib cannot give: %s", r.agent.Name, status)
	}
	return failed("%s: the task ended %s: %s", r.agent.Name, stateName(task.Status.State), status)
}

// footer names the conversation an answer belongs to, and the task that
// waits for input, so the next call can go on with them.
func footer(conversation string, waiting a2a.TaskID) string {
	switch {
	case waiting != "":
		return fmt.Sprintf("\n\n(The agent waits for an answer: send it with conversation %q and task %q.)", conversation, waiting)
	case conversation != "":
		return fmt.Sprintf("\n\n(Conversation %q; pass it to follow up.)", conversation)
	}
	return ""
}

// stateName is a task state as a person reads it: TASK_STATE_FAILED is "failed".
func stateName(s a2a.TaskState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s.String(), "TASK_STATE_")), "_", " ")
}

// text joins the text of parts and names the parts that are not text.
func text(parts a2a.ContentParts) string {
	var out []string
	for _, p := range parts {
		if p == nil {
			continue
		}
		if t := p.Text(); t != "" {
			out = append(out, t)
			continue
		}
		out = append(out, fmt.Sprintf("[a %s part nib does not show%s]", strings.TrimSpace(p.MediaType+" "), name(p.Filename)))
	}
	return strings.Join(out, "\n")
}

// name is ", named f" when f is set.
func name(f string) string {
	if f == "" {
		return ""
	}
	return ", named " + clean(f, 80)
}

// failed is an error result.
func failed(format string, args ...any) agent.ToolResult {
	return agent.ToolResult{Content: "Error: " + fmt.Sprintf(format, args...), IsError: true}
}
