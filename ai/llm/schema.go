package llm

import (
	"cmp"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Struct tags [SchemaFor] reads besides `json`. On a slice field enum,
// min and max constrain the elements.
const (
	tagDescription = "desc" // the parameter's description
	tagEnum        = "enum" // comma-separated allowed values
	tagMinimum     = "min"  // inclusive lower bound
	tagMaximum     = "max"  // inclusive upper bound
)

// SchemaFor derives a tool's parameter schema from the struct T, naming
// fields as encoding/json does. A field is required unless omitempty or
// omitzero; a required pointer may be null. Undescribable types fail.
func SchemaFor[T any]() (FunctionParams, error) {
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		return FunctionParams{}, fmt.Errorf("schema: %s is not a struct", t)
	}
	object, err := objectOf(t, nil)
	if err != nil {
		return FunctionParams{}, fmt.Errorf("schema: %w", err)
	}
	return object.params(), nil
}

var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
var jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()

// descend adds t to path, the types being described, or reports that t
// contains itself. Each branch gets its own path, so siblings may share a type.
func descend(path []reflect.Type, t reflect.Type) ([]reflect.Type, error) {
	if slices.Contains(path, t) {
		return nil, fmt.Errorf("%s contains itself", t)
	}
	return append(slices.Clip(path), t), nil
}

// selfDecoding reports whether t's own method reads its JSON, so that
// nothing derived from its fields would describe what it accepts.
func selfDecoding(t reflect.Type) bool {
	pointer := reflect.PointerTo(t)
	return pointer.Implements(jsonUnmarshaler) || pointer.Implements(textUnmarshaler)
}

func errSelfDecoding(t reflect.Type) error {
	return fmt.Errorf("%s decodes itself, so its schema is unknown", t)
}

// paramOf maps a Go type to its schema; path is the types it is inside.
func paramOf(t reflect.Type, path []reflect.Type) (FunctionParam, error) {
	switch {
	case reflect.PointerTo(t).Implements(textUnmarshaler):
		return FunctionParam{Type: "string"}, nil
	case selfDecoding(t):
		return FunctionParam{}, errSelfDecoding(t)
	case t.Kind() == reflect.Struct:
		return objectOf(t, path)
	}
	path, err := descend(path, t)
	if err != nil {
		return FunctionParam{}, err
	}
	switch t.Kind() {
	case reflect.Pointer:
		p, err := paramOf(t.Elem(), path)
		p.Nullable = true
		return p, err
	case reflect.String:
		return FunctionParam{Type: "string"}, nil
	case reflect.Bool:
		return FunctionParam{Type: "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return FunctionParam{Type: "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return FunctionParam{Type: "number"}, nil
	case reflect.Slice, reflect.Array:
		// encoding/json reads a byte slice from a base64 string.
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return FunctionParam{Type: "string"}, nil
		}
		items, err := paramOf(t.Elem(), path)
		return FunctionParam{Type: "array", Items: &items}, err
	case reflect.Map:
		// The schema type cannot describe a map's values, so their
		// constraints would go unchecked; refuse rather than accept anything.
		return FunctionParam{}, fmt.Errorf("%s: maps are not supported", t)
	}
	return FunctionParam{}, fmt.Errorf("%s has no JSON schema", t)
}

// objectOf describes a struct: one property per exported field, embedded
// structs flattened as encoding/json flattens them. Two fields with one
// name are an error rather than a guess at which one json would pick.
func objectOf(t reflect.Type, path []reflect.Type) (FunctionParam, error) {
	// Checked here too: the root and embedded structs do not pass paramOf.
	if selfDecoding(t) {
		return FunctionParam{}, errSelfDecoding(t)
	}
	path, err := descend(path, t)
	if err != nil {
		return FunctionParam{}, err
	}
	object := FunctionParam{Type: "object", Properties: map[string]FunctionParam{}}
	add := func(name string, p FunctionParam, required bool) error {
		if _, taken := object.Properties[name]; taken {
			return fmt.Errorf("%s: two fields are named %q", t, name)
		}
		object.Properties[name] = p
		if required {
			object.Required = append(object.Required, name)
		}
		return nil
	}
	for i := range t.NumField() {
		field := t.Field(i)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if embedded, ok := embeddedStruct(field, name); ok {
			inner, err := objectOf(embedded, path)
			if err != nil {
				return FunctionParam{}, err
			}
			for innerName, p := range inner.Properties {
				if err := add(innerName, p, slices.Contains(inner.Required, innerName)); err != nil {
					return FunctionParam{}, err
				}
			}
			continue
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		p, err := paramOf(field.Type, path)
		if err == nil {
			err = constrain(&p, field.Tag)
		}
		if err != nil {
			return FunctionParam{}, fmt.Errorf("%s.%s: %w", t.Name(), field.Name, err)
		}
		p.Description = field.Tag.Get(tagDescription)
		optional := strings.Split(options, ",")
		// With it json reads the value from a string the schema would refuse.
		if slices.Contains(optional, "string") {
			return FunctionParam{}, fmt.Errorf("%s.%s: the json string option is not supported", t.Name(), field.Name)
		}
		required := !slices.Contains(optional, "omitempty") && !slices.Contains(optional, "omitzero")
		// An optional field is left out, not sent as null: no union needed.
		p.Nullable = p.Nullable && required
		if err := add(name, p, required); err != nil {
			return FunctionParam{}, err
		}
	}
	// Promoted fields arrive in map order; keep the schema stable.
	slices.Sort(object.Required)
	return object, nil
}

// embeddedStruct returns the struct type an untagged embedded field
// promotes its fields from, through a pointer if need be.
func embeddedStruct(field reflect.StructField, jsonName string) (reflect.Type, bool) {
	t := field.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t, field.Anonymous && jsonName == "" && t.Kind() == reflect.Struct
}

// constrain applies a field's enum, min and max tags to p, or to its
// elements when p is an array.
func constrain(p *FunctionParam, tag reflect.StructTag) (err error) {
	target := p
	for target.Type == "array" {
		target = target.Items
	}
	if list, ok := tag.Lookup(tagEnum); ok {
		for value := range strings.SplitSeq(list, ",") {
			parsed, err := enumValue(target.Type, strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("enum %q: %w", value, err)
			}
			target.Enum = append(target.Enum, parsed)
		}
	}
	if target.Minimum, err = boundTag(tag, tagMinimum, target.Type); err != nil {
		return err
	}
	target.Maximum, err = boundTag(tag, tagMaximum, target.Type)
	return err
}

// boundTag parses a field's min or max tag; nil when it has none.
func boundTag(tag reflect.StructTag, name, schemaType string) (*float64, error) {
	text, ok := tag.Lookup(name)
	if !ok {
		return nil, nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || schemaType != "integer" && schemaType != "number" {
		return nil, fmt.Errorf("%s %q: want a number on a numeric field", name, text)
	}
	return &value, nil
}

// enumValue parses one enum tag entry as the schema type it belongs to.
func enumValue(schemaType, text string) (any, error) {
	switch schemaType {
	case "string":
		return text, nil
	case "integer":
		return strconv.ParseInt(text, 10, 64)
	case "number":
		return strconv.ParseFloat(text, 64)
	}
	return nil, fmt.Errorf("a %s field cannot have an enum", schemaType)
}

// DecodeArguments checks a tool call's raw JSON arguments against params
// (see [FunctionParams.Validate]) and decodes them into A.
func DecodeArguments[A any](params FunctionParams, arguments string) (A, error) {
	var args A
	value, err := params.parse(arguments)
	if err != nil {
		return args, err
	}
	// Decode the value that was checked, not the raw text: where a member
	// is repeated, json would build the struct from more than the last one.
	checked, err := json.Marshal(value)
	if err != nil {
		return args, err
	}
	err = json.Unmarshal(checked, &args)
	return args, err
}

// Validate checks a tool call's raw JSON arguments against the schema and
// reports every violation, joined. Empty arguments count as {}, and null
// for an optional field counts as leaving it out, as it does to encoding/json.
func (p FunctionParams) Validate(arguments string) error {
	_, err := p.parse(arguments)
	return err
}

// parse decodes arguments and checks them against the schema. Of a
// repeated member it sees the last; [DecodeArguments] decodes the same.
func (p FunctionParams) parse(arguments string) (any, error) {
	// Some models send "" for a call with no arguments.
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("arguments are not valid JSON: data after the first value")
	}
	return value, errors.Join(p.object().check("", value)...)
}

// check returns what is wrong with value under this schema; at names the
// value for the model, empty for the arguments as a whole. A schema with
// no Type accepts anything.
func (p FunctionParam) check(at string, value any) []error {
	if value == nil {
		if p.Nullable || p.Type == "" {
			return nil
		}
		return failAt(at, "must not be null")
	}
	if len(p.Enum) > 0 && !slices.ContainsFunc(p.Enum, func(allowed any) bool { return sameJSON(allowed, value) }) {
		return failAt(at, "%s is not one of %s", inJSON(value), strings.Trim(inJSON(p.Enum), "[]"))
	}
	got := jsonType(value)
	switch {
	case p.Type == "":
		return nil
	case got != p.Type && (got != "number" || p.Type != "integer"):
		return failAt(at, "want %s, got %s", p.Type, got)
	}
	switch v := value.(type) {
	case json.Number:
		return p.checkNumber(at, v)
	case map[string]any:
		return p.checkObject(at, v)
	case []any:
		if p.Items == nil {
			return nil
		}
		var errs []error
		for i, item := range v {
			errs = append(errs, p.Items.check(fmt.Sprintf("%s[%d]", at, i), item)...)
		}
		return errs
	}
	return nil
}

// jsonType names the JSON Schema type of a decoded, non-null value.
func jsonType(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case []any:
		return "array"
	}
	return "object"
}

// checkNumber checks a JSON number against an integer or number schema.
func (p FunctionParam) checkNumber(at string, n json.Number) []error {
	float, err := n.Float64()
	// Any whole number passes; decoding enforces the Go field's range.
	whole, isWhole := new(big.Int).SetString(n.String(), 10)
	if err != nil || p.Type == "integer" && !isWhole {
		return failAt(at, "%s is not a valid %s", n, p.Type)
	}
	// compare is the sign of n minus bound. A number is the float64 the
	// field will hold; an integer that large would round onto its bound.
	compare := func(bound float64) int { return cmp.Compare(float, bound) }
	if p.Type == "integer" {
		compare = func(bound float64) int { return compareExactly(whole, bound) }
	}
	switch {
	case p.Minimum != nil && compare(*p.Minimum) < 0:
		return failAt(at, "%s is below the minimum %v", n, *p.Minimum)
	case p.Maximum != nil && compare(*p.Maximum) > 0:
		return failAt(at, "%s is above the maximum %v", n, *p.Maximum)
	}
	return nil
}

// compareExactly is the sign of n minus bound, with no rounding of n. A
// non-finite bound limits nothing.
func compareExactly(n *big.Int, bound float64) int {
	limit := new(big.Rat).SetFloat64(bound)
	if limit == nil {
		return 0
	}
	return new(big.Rat).SetInt(n).Cmp(limit)
}

// checkObject checks an object's fields in name order, so the report is
// stable. A schema without Properties accepts any fields.
func (p FunctionParam) checkObject(at string, object map[string]any) []error {
	var errs []error
	for _, name := range p.Required {
		if _, present := object[name]; !present {
			errs = append(errs, failAt(fieldAt(at, name), "required")...)
		}
	}
	closed := p.AdditionalProperties != nil && !*p.AdditionalProperties
	for _, name := range slices.Sorted(maps.Keys(object)) {
		property, known := p.Properties[name]
		if !known {
			// json matches names without regard to case, so this one would
			// reach the field unchecked.
			if alias := p.aliasOf(name); alias != "" {
				errs = append(errs, failAt(fieldAt(at, name), "unknown field; did you mean %q?", alias)...)
			} else if closed {
				errs = append(errs, failAt(fieldAt(at, name), "unknown field")...)
			}
			continue
		}
		// A null optional field counts as left out, as encoding/json reads it.
		if object[name] == nil && !slices.Contains(p.Required, name) {
			continue
		}
		errs = append(errs, property.check(fieldAt(at, name), object[name])...)
	}
	return errs
}

// aliasOf returns the property that name spells in another case, if any.
func (p FunctionParam) aliasOf(name string) string {
	for property := range p.Properties {
		if strings.EqualFold(property, name) {
			return property
		}
	}
	return ""
}

// failAt is one violation at the named value, worded for the model.
func failAt(at, format string, args ...any) []error {
	return []error{fmt.Errorf("%s: %s", cmp.Or(at, "arguments"), fmt.Sprintf(format, args...))}
}

// fieldAt names a field of the object at parent.
func fieldAt(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

// sameJSON reports whether an enum entry and a decoded value are the same
// JSON value. Numbers compare exactly and by value, so 1 matches 1.0.
func sameJSON(allowed, value any) bool {
	if number, ok := value.(json.Number); ok {
		// Past float64's range it is no valid argument, and costly to parse.
		if _, err := number.Float64(); err != nil {
			return false
		}
		got, gotOK := new(big.Rat).SetString(number.String())
		want, wantOK := new(big.Rat).SetString(inJSON(allowed))
		return gotOK && wantOK && got.Cmp(want) == 0
	}
	return inJSON(allowed) == inJSON(value)
}

// inJSON renders v as JSON for comparison and for messages to the model.
func inJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}
