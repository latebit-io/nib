package repl

import (
	"context"
	"strings"

	"github.com/latebit-io/nib/kit/permission"
)

// interaction is a question the session puts to the person for a caller
// that waits: shown once, answered line by line.
type interaction interface {
	// show puts the question on screen.
	show(r *REPL)
	// take handles one line and reports whether the question is answered.
	take(r *REPL, line string) bool
	// dismiss answers for a person who can no longer answer.
	dismiss(r *REPL)
}

// put hands in to the loop. It reports false when ctx ended first or
// the session is over.
func (r *REPL) put(ctx context.Context, in interaction) bool {
	select {
	case r.asks <- in:
		return true
	case <-r.stopped:
	case <-ctx.Done():
	}
	return false
}

// refuse dismisses the waiting question: the input has ended and
// nobody is left to answer.
func (r *REPL) refuse() {
	if r.asking == nil {
		return
	}
	r.asking.dismiss(r)
	r.asking = nil
}

// question is one approval request and where its answer goes.
type question struct {
	req    permission.Request
	answer chan permission.Decision // room for one: the loop never waits on it
}

// Ask puts an approval request to the person and waits for the answer.
// It is the session's [permission.Asker]; unlike every other method it
// is called from the agent's goroutine, and hands over to the loop.
func (r *REPL) Ask(ctx context.Context, req permission.Request) (permission.Decision, error) {
	q := &question{req: req, answer: make(chan permission.Decision, 1)}
	if !r.put(ctx, q) {
		return permission.Deny, ctx.Err()
	}
	select {
	case d := <-q.answer:
		return d, nil
	case <-ctx.Done():
		return permission.Deny, ctx.Err()
	}
}

func (q *question) show(r *REPL) {
	line := "? " + q.req.Tool + " wants to run: " + q.req.Detail
	if q.req.Reason != "" {
		line += "  (" + q.req.Reason + ")"
	}
	r.println(line)
	r.print("  allow? [y]es, [a]lways, [n]o: ")
}

// take accepts a clear answer; anything else asks again.
func (q *question) take(r *REPL, line string) bool {
	switch strings.ToLower(line) {
	case "y", "yes":
		q.answer <- permission.Allow
	case "a", "always":
		q.answer <- permission.AllowAlways
	case "n", "no":
		q.answer <- permission.Deny
	default:
		q.show(r)
		return false
	}
	return true
}

func (q *question) dismiss(r *REPL) {
	r.println("(no input left to answer with; refused)")
	q.answer <- permission.Deny
}
