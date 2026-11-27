package elicit

import (
	"reflect"
	"testing"
)

// TestParseFields: fields come in name order, with every kind a flat
// form allows.
func TestParseFields(t *testing.T) {
	fields, err := ParseFields([]byte(`{"type":"object","required":["name","age"],"properties":{
		"name":{"type":"string","title":"Your name","minLength":2},
		"age":{"type":"integer","minimum":0,"maximum":150},
		"email":{"type":"string","format":"email"},
		"agree":{"type":"boolean","default":true},
		"color":{"type":"string","oneOf":[{"const":"r","title":"Red"},{"const":"g","title":"Green"}]},
		"size":{"type":"string","enum":["s","m"],"enumNames":["Small","Medium"]},
		"tags":{"type":"array","items":{"enum":["a","b","c"]},"maxItems":2}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range fields {
		order = append(order, f.Name)
	}
	if want := []string{"age", "agree", "color", "email", "name", "size", "tags"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if !byName["name"].Required || byName["email"].Required || byName["agree"].Default != true || byName["name"].Label() != "Your name" {
		t.Errorf("required, default or title lost: %+v", fields)
	}
	if !reflect.DeepEqual(byName["color"].Labels, []string{"Red", "Green"}) || !reflect.DeepEqual(byName["size"].Labels, []string{"Small", "Medium"}) || !byName["tags"].Multiple {
		t.Errorf("choices = %+v %+v %+v", byName["color"], byName["size"], byName["tags"])
	}
}

func TestField_Parse(t *testing.T) {
	fields, err := ParseFields([]byte(`{"properties":{
		"name":{"type":"string","minLength":2},
		"age":{"type":"integer","minimum":0},
		"ratio":{"type":"number","maximum":1},
		"agree":{"type":"boolean"},
		"email":{"type":"string","format":"email"},
		"when":{"type":"string","format":"date"},
		"color":{"type":"string","oneOf":[{"const":"r","title":"Red"},{"const":"g","title":"Green"}]},
		"tags":{"type":"array","items":{"enum":["a","b","c"]},"maxItems":2}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	f := map[string]Field{}
	for _, field := range fields {
		f[field.Name] = field
	}
	for _, c := range []struct {
		field, in string
		want      any
	}{
		{"name", "Ada", "Ada"},
		{"age", "36", int64(36)},
		{"ratio", "0.5", 0.5},
		{"agree", "yes", true},
		{"agree", "N", false},
		{"email", "ada@example.com", "ada@example.com"},
		{"when", "1815-12-10", "1815-12-10"},
		{"color", "green", "g"},
		{"color", "1", "r"},
		{"tags", "a, c", []string{"a", "c"}},
	} {
		got, err := f[c.field].Parse(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s.Parse(%q) = %v (%T), %v; want %v", c.field, c.in, got, got, err, c.want)
		}
	}
	for field, in := range map[string]string{
		"name": "A", "age": "-1", "ratio": "2", "agree": "maybe", "email": "nope",
		"when": "10/12/1815", "color": "blue", "tags": "a,b,c",
	} {
		if _, err := f[field].Parse(in); err == nil {
			t.Errorf("%s.Parse(%q) must fail", field, in)
		}
	}
}
