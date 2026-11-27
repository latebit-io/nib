package repl

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/latebit-io/nib/kit/elicit"
)

// form is a question from outside the session (an MCP server) and the
// person's answers so far.
type form struct {
	req    elicit.Request
	answer chan elicit.Result // room for one: the loop never waits on it
	next   int                // the field being asked; len(Fields) means the review
	values map[string]any     // answers by field name
}

// Elicit puts a server's question to the person and waits for the
// answer. It is the session's [elicit.Asker]; like [REPL.Ask] it is
// called from another goroutine and hands over to the loop.
func (r *REPL) Elicit(ctx context.Context, req elicit.Request) (elicit.Result, error) {
	f := &form{req: req, answer: make(chan elicit.Result, 1), values: map[string]any{}}
	if !r.put(ctx, f) {
		return elicit.Result{Action: elicit.Cancel}, ctx.Err()
	}
	select {
	case res := <-f.answer:
		return res, nil
	case <-ctx.Done():
		return elicit.Result{}, ctx.Err()
	}
}

// show introduces the question and asks the first thing.
func (f *form) show(r *REPL) {
	from, message := printable(f.req.From), printable(f.req.Message)
	if f.req.Mode != elicit.Page {
		r.println("? " + from + " asks: " + message)
		f.ask(r)
		return
	}
	r.println("? " + from + " asks you to open a page: " + message)
	r.println("  " + f.req.URL)
	host := ""
	if u, err := url.Parse(f.req.URL); err == nil {
		host = u.Hostname()
	}
	r.println("  site: " + host)
	if strings.Contains(host, "xn--") {
		r.println("  warning: the site's name is encoded (xn--); check it is the one you expect")
	}
	f.ask(r)
}

// ask asks for the current field, or for the review.
func (f *form) ask(r *REPL) {
	switch {
	case f.req.Mode == elicit.Page:
		r.print("  open it in your browser? [y]es, [n]o: ")
	case f.next < len(f.req.Fields):
		r.print("  " + fieldPrompt(f.req.Fields[f.next], f.values) + ": ")
	default:
		for _, field := range f.req.Fields {
			if v, ok := f.values[field.Name]; ok {
				r.println(fmt.Sprintf("  %s = %v", printable(field.Label()), v))
			}
		}
		r.print("  send? [y]es, [e]dit, [d]ecline, [c]ancel: ")
	}
}

// fieldPrompt names a field and how to answer it.
func fieldPrompt(field elicit.Field, values map[string]any) string {
	var hints []string
	switch {
	case len(field.Options) > 0:
		opts := make([]string, len(field.Options))
		for i, l := range field.Labels {
			opts[i] = strconv.Itoa(i+1) + " " + printable(l)
		}
		hint := strings.Join(opts, ", ")
		if field.Multiple {
			hint += "; several with commas"
		}
		hints = append(hints, hint)
	case field.Type == "boolean":
		hints = append(hints, "yes or no")
	case field.Format != "":
		hints = append(hints, field.Format)
	case field.Type != "string":
		hints = append(hints, field.Type)
	}
	if v, ok := values[field.Name]; ok {
		hints = append(hints, fmt.Sprintf("Enter keeps %v", v))
	} else if field.Default != nil {
		hints = append(hints, fmt.Sprintf("Enter means %v", field.Default))
	} else if !field.Required {
		hints = append(hints, "optional")
	}
	text := printable(field.Label())
	if field.Description != "" {
		text += " (" + printable(field.Description) + ")"
	}
	if len(hints) > 0 {
		text += " [" + strings.Join(hints, "; ") + "]"
	}
	return text
}

// take handles one line of the person's answer.
func (f *form) take(r *REPL, line string) bool {
	if f.req.Mode == elicit.Page {
		switch strings.ToLower(line) {
		case "y", "yes":
			// The page opens in the browser, out of the model's reach.
			if err := r.openURL(f.req.URL); err != nil {
				r.println("  no browser opened (" + err.Error() + "); open the address by hand")
			}
			return f.done(elicit.Result{Action: elicit.Accept})
		case "n", "no":
			return f.done(elicit.Result{Action: elicit.Decline})
		}
		f.ask(r)
		return false
	}
	if f.next < len(f.req.Fields) {
		f.fill(r, f.req.Fields[f.next], line)
		f.ask(r)
		return false
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return f.done(elicit.Result{Action: elicit.Accept, Content: f.values})
	case "e", "edit":
		f.next = 0
	case "d", "decline":
		return f.done(elicit.Result{Action: elicit.Decline})
	case "c", "cancel":
		return f.done(elicit.Result{Action: elicit.Cancel})
	}
	f.ask(r)
	return false
}

// fill records an answer to one field, or says what is wrong with it
// and stays on the field.
func (f *form) fill(r *REPL, field elicit.Field, line string) {
	if line == "" {
		_, kept := f.values[field.Name]
		switch {
		case kept:
		case field.Default != nil:
			f.values[field.Name] = field.Default
		case field.Required:
			r.println("  an answer is required")
			return
		}
		f.next++
		return
	}
	v, err := field.Parse(line)
	if err != nil {
		r.println("  " + err.Error())
		return
	}
	f.values[field.Name] = v
	f.next++
}

// printable makes a server's text safe to print: a control character
// (a newline, an escape sequence) or a direction override could forge
// a line of the question, so each becomes a space.
func printable(text string) string {
	return strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Bidi_Control, c) {
			return ' '
		}
		return c
	}, text)
}

// done delivers the answer and reports the question over.
func (f *form) done(res elicit.Result) bool {
	f.answer <- res
	return true
}

func (f *form) dismiss(r *REPL) {
	r.println("(no input left to answer with; dismissed)")
	f.done(elicit.Result{Action: elicit.Cancel})
}
