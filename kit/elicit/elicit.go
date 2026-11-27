// Package elicit is a question an outside party (an MCP server) puts
// to the person during a call: a flat form or a page to open. It is the
// port between that party and a frontend, which imports only this.
package elicit

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Modes of a request.
const (
	// Form asks the person to fill in [Request.Fields].
	Form = "form"
	// Page asks the person to open [Request.URL] in a browser, for what
	// must not pass through the client, such as a password.
	Page = "url"
)

// Actions: the person's answer.
const (
	// Accept: the person answered (form) or agreed to open the page.
	Accept = "accept"
	// Decline: the person said no.
	Decline = "decline"
	// Cancel: the question was dismissed without a choice.
	Cancel = "cancel"
)

// Request is a question put to the person.
type Request struct {
	// From names who asks, which the person must be shown.
	From string
	// Mode is [Form] or [Page].
	Mode string
	// Message says what is wanted and why.
	Message string
	// Fields are the form's fields.
	Fields []Field
	// URL is the page to open, in [Page] mode.
	URL string
}

// Result is the person's answer.
type Result struct {
	// Action is [Accept], [Decline] or [Cancel].
	Action string
	// Content maps field names to the values from [Field.Parse], for an
	// accepted form.
	Content map[string]any
}

// Asker puts a request to the person. It is a callback, not an event:
// the call waits on it.
type Asker func(ctx context.Context, r Request) (Result, error)

// Field is one field of a form. Forms are flat: a text, a number, a yes
// or no, or a choice from a list.
type Field struct {
	Name        string
	Title       string
	Description string
	// Type is "string", "number", "integer" or "boolean".
	Type     string
	Required bool
	// Format restricts a string: "email", "uri", "date" or "date-time".
	Format string
	// Options are the allowed values of a choice, with Labels to show.
	Options []string
	Labels  []string
	// Multiple lets a choice take several options.
	Multiple bool
	// Default is the value an empty answer means, when set.
	Default any

	minimum, maximum     *float64
	minLength, maxLength *int
	minItems, maxItems   *int
}

// Label is the name a field is shown by.
func (f Field) Label() string { return cmp.Or(f.Title, f.Name) }

// fieldSchema is one property of a requested schema.
type fieldSchema struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Format      string   `json:"format"`
	Enum        []string `json:"enum"`
	EnumNames   []string `json:"enumNames"`
	OneOf       []choice `json:"oneOf"`
	Items       *struct {
		Enum  []string `json:"enum"`
		AnyOf []choice `json:"anyOf"`
	} `json:"items"`
	Default   any      `json:"default"`
	Minimum   *float64 `json:"minimum"`
	Maximum   *float64 `json:"maximum"`
	MinLength *int     `json:"minLength"`
	MaxLength *int     `json:"maxLength"`
	MinItems  *int     `json:"minItems"`
	MaxItems  *int     `json:"maxItems"`
}

// choice is one titled option.
type choice struct {
	Const string `json:"const"`
	Title string `json:"title"`
}

// ParseFields reads a flat form schema. Fields come in name order: the
// schema reaches here as a decoded map, which keeps no order.
func ParseFields(schema []byte) ([]Field, error) {
	var top struct {
		Properties map[string]fieldSchema `json:"properties"`
		Required   []string               `json:"required"`
	}
	if err := json.Unmarshal(schema, &top); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	fields := make([]Field, 0, len(top.Properties))
	for _, name := range slices.Sorted(maps.Keys(top.Properties)) {
		f := newField(name, top.Properties[name])
		f.Required = slices.Contains(top.Required, name)
		fields = append(fields, f)
	}
	return fields, nil
}

// newField builds a field from its schema. The schema was checked by
// the protocol layer: an array is always a multiple choice.
func newField(name string, s fieldSchema) Field {
	f := Field{
		Name: name, Title: s.Title, Description: s.Description, Type: s.Type, Format: s.Format,
		Default: s.Default, minimum: s.Minimum, maximum: s.Maximum,
		minLength: s.MinLength, maxLength: s.MaxLength, minItems: s.MinItems, maxItems: s.MaxItems,
	}
	f.Options, f.Labels = options(s.Enum, s.EnumNames, s.OneOf)
	if s.Type == "array" && s.Items != nil {
		f.Type, f.Multiple = "string", true
		f.Options, f.Labels = options(s.Items.Enum, nil, s.Items.AnyOf)
	}
	return f
}

// options returns a choice's values and labels from either form a
// schema may use: enum (with optional names), or const and title pairs.
func options(enum, names []string, titled []choice) (values, labels []string) {
	if len(titled) > 0 {
		for _, c := range titled {
			values = append(values, c.Const)
			labels = append(labels, cmp.Or(c.Title, c.Const))
		}
		return values, labels
	}
	for i, v := range enum {
		values = append(values, v)
		label := v
		if i < len(names) && names[i] != "" {
			label = names[i]
		}
		labels = append(labels, label)
	}
	return values, labels
}

// Parse converts the person's answer to the field's type and checks it
// against the schema. An option may be given by value, label or number
// from 1; several are separated by commas.
func (f Field) Parse(text string) (any, error) {
	text = strings.TrimSpace(text)
	if f.Multiple {
		var picked []string
		for part := range strings.SplitSeq(text, ",") {
			v, err := f.option(strings.TrimSpace(part))
			if err != nil {
				return nil, err
			}
			picked = append(picked, v)
		}
		if err := f.count(len(picked)); err != nil {
			return nil, err
		}
		return picked, nil
	}
	if len(f.Options) > 0 {
		return f.option(text)
	}
	switch f.Type {
	case "boolean":
		switch strings.ToLower(text) {
		case "y", "yes", "true":
			return true, nil
		case "n", "no", "false":
			return false, nil
		}
		return nil, errors.New("answer yes or no")
	case "integer":
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, errors.New("a whole number is needed")
		}
		return n, f.bounds(float64(n))
	case "number":
		n, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, errors.New("a number is needed")
		}
		return n, f.bounds(n)
	}
	return text, f.checkString(text)
}

// option resolves one answer to an option's value.
func (f Field) option(answer string) (string, error) {
	for i, v := range f.Options {
		if strings.EqualFold(answer, v) || strings.EqualFold(answer, f.Labels[i]) || answer == strconv.Itoa(i+1) {
			return v, nil
		}
	}
	return "", fmt.Errorf("%q is not one of the options", answer)
}

// count checks how many options were picked.
func (f Field) count(n int) error {
	if f.minItems != nil && n < *f.minItems {
		return fmt.Errorf("pick at least %d", *f.minItems)
	}
	if f.maxItems != nil && n > *f.maxItems {
		return fmt.Errorf("pick at most %d", *f.maxItems)
	}
	return nil
}

// bounds checks a number against the minimum and maximum.
func (f Field) bounds(n float64) error {
	if f.minimum != nil && n < *f.minimum {
		return fmt.Errorf("at least %v", *f.minimum)
	}
	if f.maximum != nil && n > *f.maximum {
		return fmt.Errorf("at most %v", *f.maximum)
	}
	return nil
}

// checkString checks a text's length and format.
func (f Field) checkString(s string) error {
	n := len([]rune(s))
	if f.minLength != nil && n < *f.minLength {
		return fmt.Errorf("at least %d characters", *f.minLength)
	}
	if f.maxLength != nil && n > *f.maxLength {
		return fmt.Errorf("at most %d characters", *f.maxLength)
	}
	var err error
	switch f.Format {
	case "email":
		_, err = mail.ParseAddress(s)
	case "uri":
		var u *url.URL
		if u, err = url.Parse(s); err == nil && u.Scheme == "" {
			err = errors.New("no scheme")
		}
	case "date":
		_, err = time.Parse(time.DateOnly, s)
	case "date-time":
		_, err = time.Parse(time.RFC3339, s)
	}
	if err != nil {
		return fmt.Errorf("not a valid %s", f.Format)
	}
	return nil
}
