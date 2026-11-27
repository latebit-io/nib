package llm

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestFunctionParam_JSON: each schema marshals to exactly the JSON a
// provider is sent, and reads back as the same value.
func TestFunctionParam_JSON(t *testing.T) {
	tests := []struct {
		name  string
		param FunctionParam
		want  string
	}{
		{
			name:  "a scalar keeps the minimal encoding",
			param: FunctionParam{Type: "string", Description: "d"},
			want:  `{"type":"string","description":"d"}`,
		},
		{
			name:  "nullable bounded integer",
			param: FunctionParam{Type: "integer", Nullable: true, Minimum: new(0.0), Maximum: new(5.0)},
			want:  `{"type":["integer","null"],"description":"","minimum":0,"maximum":5}`,
		},
		{
			name:  "enum",
			param: FunctionParam{Type: "string", Enum: []any{"low", "high"}},
			want:  `{"type":"string","description":"","enum":["low","high"]}`,
		},
		{
			name:  "null joins a nullable enum",
			param: FunctionParam{Type: "string", Nullable: true, Enum: []any{"low", "high"}},
			want:  `{"type":["string","null"],"description":"","enum":["low","high",null]}`,
		},
		{
			name: "array of closed objects",
			param: FunctionParam{Type: "array", Items: &FunctionParam{
				Type:                 "object",
				Properties:           map[string]FunctionParam{"n": {Type: "number", Nullable: true}},
				Required:             []string{"n"},
				AdditionalProperties: new(false),
			}},
			want: `{"type":"array","description":"","items":{"type":"object","description":"",` +
				`"properties":{"n":{"type":["number","null"],"description":""}},"required":["n"],"additionalProperties":false}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.param)
			if err != nil || string(got) != tc.want {
				t.Fatalf("Marshal = %s, %v\nwant %s", got, err, tc.want)
			}
			var back FunctionParam
			if err := json.Unmarshal(got, &back); err != nil || !reflect.DeepEqual(back, tc.param) {
				t.Errorf("Unmarshal = %+v, %v\nwant %+v", back, err, tc.param)
			}
		})
	}
}

// TestFunctionParam_UnmarshalOneTypeList: a list of one type is that type,
// and does not make the parameter nullable.
func TestFunctionParam_UnmarshalOneTypeList(t *testing.T) {
	var p FunctionParam
	err := json.Unmarshal([]byte(`{"type":["string"],"enum":["a",null]}`), &p)
	want := FunctionParam{Type: "string", Enum: []any{"a", nil}}
	if err != nil || !reflect.DeepEqual(p, want) {
		t.Errorf("Unmarshal = %+v, %v; want %+v", p, err, want)
	}
}

// TestFunctionParam_UnmarshalPlainTypeClearsNullable: decoding into a
// reused value must not keep the union it held before.
func TestFunctionParam_UnmarshalPlainTypeClearsNullable(t *testing.T) {
	p := FunctionParam{Type: "string", Nullable: true}
	if err := json.Unmarshal([]byte(`{"type":"integer"}`), &p); err != nil || p.Type != "integer" || p.Nullable {
		t.Errorf("Unmarshal = %+v, %v; want a plain integer", p, err)
	}
	p.Nullable = true
	if err := json.Unmarshal([]byte(`{"description":"d"}`), &p); err != nil || !p.Nullable {
		t.Errorf("Unmarshal without a type = %+v, %v; want Nullable kept", p, err)
	}
}

func TestFunctionParam_UnmarshalRejectsOtherUnions(t *testing.T) {
	for _, schema := range []string{
		`{"type":["string","integer"]}`,
		`{"type":["string","integer","null"]}`,
		`{"type":["null"]}`,
		`{"type":5}`,
	} {
		var p FunctionParam
		if err := json.Unmarshal([]byte(schema), &p); err == nil {
			t.Errorf("Unmarshal(%s) = %+v; want an error", schema, p)
		}
	}
}

// TestFunctionParams_MarshalsNilAsEmpty: strict validators reject
// "required": null and "properties": null.
func TestFunctionParams_MarshalsNilAsEmpty(t *testing.T) {
	got, err := json.Marshal(ToolDef{Type: "function", Function: FunctionDef{
		Name: "t", Parameters: FunctionParams{Type: "object", AdditionalProperties: new(false)},
	}})
	want := `{"type":"function","function":{"name":"t","description":"",` +
		`"parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}}}`
	if err != nil || string(got) != want {
		t.Errorf("Marshal = %s, %v\nwant %s", got, err, want)
	}
}

type level string

type embedded struct {
	World string `json:"world,omitempty" desc:"restrict to one world"`
}

type assessment struct {
	embedded
	Query    string    `json:"query" desc:"what to look up"`
	Score    *int      `json:"score" min:"0" max:"5"`
	Level    level     `json:"level,omitempty" enum:"low, high"`
	Weights  []float64 `json:"weights,omitzero" min:"0.5"`
	Tags     []string  `json:"tags,omitempty" enum:"a,b"`
	Sources  []source  `json:"sources,omitempty"`
	Since    time.Time `json:"since,omitempty"`
	Blob     []byte    `json:"blob,omitempty"`
	Big      uint64    `json:"big,omitempty"`
	Exact    int64     `json:"exact,omitempty" enum:"9007199254740992"`
	Untagged bool
	Skipped  string `json:"-"`
	hidden   string
}

type source struct {
	URL  string `json:"url"`
	Rank *int   `json:"rank,omitempty" enum:"1,2"`
}

func TestSchemaFor(t *testing.T) {
	// Set so the linter sees the field used; the schema must skip it.
	_ = assessment{hidden: "unexported"}
	schema, err := SchemaFor[assessment]()
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	got, err := json.MarshalIndent(schema, "", " ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{
 "type": "object",
 "properties": {
  "Untagged": {"type": "boolean", "description": ""},
  "big": {"type": "integer", "description": ""},
  "blob": {"type": "string", "description": ""},
  "exact": {"type": "integer", "description": "", "enum": [9007199254740992]},
  "level": {"type": "string", "description": "", "enum": ["low", "high"]},
  "query": {"type": "string", "description": "what to look up"},
  "score": {"type": ["integer", "null"], "description": "", "minimum": 0, "maximum": 5},
  "since": {"type": "string", "description": ""},
  "sources": {"type": "array", "description": "", "items": {"type": "object", "description": "",
   "properties": {
    "rank": {"type": "integer", "description": "", "enum": [1, 2]},
    "url": {"type": "string", "description": ""}},
   "required": ["url"]}},
  "tags": {"type": "array", "description": "", "items": {"type": "string", "description": "", "enum": ["a", "b"]}},
  "weights": {"type": "array", "description": "", "items": {"type": "number", "description": "", "minimum": 0.5}},
  "world": {"type": "string", "description": "restrict to one world"}
 },
 "required": ["Untagged", "query", "score"]
}`
	if compact(t, string(got)) != compact(t, want) {
		t.Errorf("schema =\n%s\nwant\n%s", got, want)
	}
}

// TestSchemaFor_SharedTypeIsNotACycle: two fields of one type, and a type
// that recurs only in a sibling, must not read as recursion.
func TestSchemaFor_SharedTypeIsNotACycle(t *testing.T) {
	_, err := SchemaFor[struct {
		A, B source
		C    []source
		D    [][]int
	}]()
	if err != nil {
		t.Errorf("SchemaFor = %v; want a schema", err)
	}
}

// TestSchemaFor_EmptyStruct: a tool with no arguments still gets an object
// schema with an empty required array.
func TestSchemaFor_EmptyStruct(t *testing.T) {
	schema, err := SchemaFor[struct{}]()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(mustJSON(t, schema)), `{"type":"object","properties":{},"required":[]}`; got != want {
		t.Errorf("schema = %s; want %s", got, want)
	}
}

func compact(t *testing.T, text string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(text)); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	return b.String()
}

type nested []nested

// selfDecoder ignores its input: no schema could describe what it accepts.
type selfDecoder struct {
	Limit int `json:"limit" max:"25"`
}

func (s *selfDecoder) UnmarshalJSON([]byte) error {
	s.Limit = 99
	return nil
}

type loop *loop

type selfRef struct {
	Next *selfRef `json:"next"`
}

type selfEmbed struct {
	*selfEmbed
	Name string
}

func TestSchemaFor_Errors(t *testing.T) {
	_ = selfEmbed{}.selfEmbed // used, for the linter
	type derive func() (FunctionParams, error)
	tests := []struct {
		name   string
		derive derive
		want   string
	}{
		{"not a struct", SchemaFor[string], "not a struct"},
		{"any", SchemaFor[struct{ V any }], ".V: interface {} has no JSON schema"},
		{"channel", SchemaFor[struct{ C chan int }], "has no JSON schema"},
		{"map", SchemaFor[struct{ M map[string]int }], "maps are not supported"},
		{"recursive slice", SchemaFor[struct{ V nested }], "nested contains itself"},
		{"recursive pointer", SchemaFor[struct{ V loop }], "loop contains itself"},
		{"json string option", SchemaFor[struct {
			N int `json:"n,string"`
		}], "json string option"},
		{"self-decoding", SchemaFor[struct{ R json.RawMessage }], "decodes itself"},
		{"self-decoding root", SchemaFor[selfDecoder], "selfDecoder decodes itself"},
		{"self-decoding embed", SchemaFor[struct {
			selfDecoder
			Other string
		}], "decodes itself"},
		{"recursive", SchemaFor[selfRef], "contains itself"},
		{"recursive embed", SchemaFor[selfEmbed], "contains itself"},
		{"duplicate name", SchemaFor[struct {
			embedded
			W string `json:"world"`
		}], `two fields are named "world"`},
		{"enum on a boolean", SchemaFor[struct {
			B bool `enum:"true"`
		}], "cannot have an enum"},
		{"enum value of the wrong type", SchemaFor[struct {
			N int `enum:"1,x"`
		}], `enum "x"`},
		{"min on a string", SchemaFor[struct {
			S string `min:"1"`
		}], "numeric field"},
		{"max that is not a number", SchemaFor[struct {
			N int `max:"lots"`
		}], `max "lots"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.derive(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v; want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestDecodeArguments: arguments are checked before they are decoded, and
// blank arguments decode as an empty object.
func TestDecodeArguments(t *testing.T) {
	type args struct {
		Query string `json:"query,omitempty"`
		Limit *int   `json:"limit,omitempty" max:"25"`
		Depth int8   `json:"depth,omitempty" max:"5000"`
	}
	schema, err := SchemaFor[args]()
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	tests := []struct {
		name    string
		raw     string
		want    args
		wantErr string
	}{
		{name: "decoded", raw: `{"query":"q","limit":3}`, want: args{Query: "q", Limit: new(3)}},
		{name: "blank is an empty object", raw: " "},
		{name: "null optional stays unset", raw: `{"limit":null}`},
		{name: "violation", raw: `{"limit":99}`, wantErr: "limit: 99 is above the maximum 25"},
		{name: "a repeated member decodes as it was checked", raw: `{"depth":9999,"depth":null}`},
		{name: "another spelling cannot bypass the check", raw: `{"depth":1,"Depth":9999}`,
			wantErr: `Depth: unknown field; did you mean "depth"?`},
		{name: "beyond the Go field's range", raw: `{"depth":300}`, wantErr: "depth"},
		{name: "not JSON", raw: `{"query":`, wantErr: "not valid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeArguments[args](schema, tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v; want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DecodeArguments = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	open, err := SchemaFor[assessment]()
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	closed := open
	closed.AdditionalProperties = new(false)
	const ok = `"Untagged":true,"query":"q","score":null`

	tests := []struct {
		name   string
		schema FunctionParams
		args   string
		want   []string // substrings of the error; none means valid
	}{
		{name: "minimal", schema: open, args: `{` + ok + `}`},
		{name: "everything", schema: closed, args: `{` + ok + `,"score":5,"level":"high","weights":[0.5,2],
			"tags":["a"],"sources":[{"url":"u","rank":2},{"url":"v","rank":null}],"since":"2026-01-02T03:04:05Z",
			"big":18446744073709551615,"exact":9007199254740992,"world":"w"}`},
		{name: "unknown field allowed when open", schema: open, args: `{` + ok + `,"other":1}`},
		{name: "unknown field refused when closed", schema: closed, args: `{` + ok + `,"other":1}`,
			want: []string{"other: unknown field"}},
		{name: "empty arguments are an empty object", schema: open, args: " ",
			want: []string{"Untagged: required", "query: required", "score: required"}},
		{name: "not JSON", schema: open, args: `{"query":`, want: []string{"not valid JSON"}},
		{name: "data after the object", schema: open, args: `{` + ok + `} {}`, want: []string{"data after the first value"}},
		{name: "not an object", schema: open, args: `[1]`, want: []string{"arguments: want object, got array"}},
		{name: "null for an optional field is leaving it out", schema: open, args: `{` + ok + `,"level":null,"tags":null}`},
		{name: "null where not nullable", schema: open, args: `{"Untagged":true,"query":null,"score":1}`,
			want: []string{"query: must not be null"}},
		{name: "wrong types", schema: open, args: `{"Untagged":"yes","query":7,"score":"3"}`,
			want: []string{"Untagged: want boolean, got string", "query: want string, got number", "score: want integer, got string"}},
		{name: "fraction for an integer", schema: open, args: `{"Untagged":true,"query":"q","score":2.5}`,
			want: []string{"score: 2.5 is not a valid integer"}},
		{name: "below minimum", schema: open, args: `{"Untagged":true,"query":"q","score":-1}`,
			want: []string{"score: -1 is below the minimum 0"}},
		{name: "above maximum", schema: open, args: `{"Untagged":true,"query":"q","score":6}`,
			want: []string{"score: 6 is above the maximum 5"}},
		{name: "outside enum", schema: open, args: `{` + ok + `,"level":"mid"}`,
			want: []string{`level: "mid" is not one of "low","high"`}},
		{name: "array elements are checked", schema: open, args: `{` + ok + `,"tags":["a","z"],"weights":[1,0.1]}`,
			want: []string{`tags[1]: "z" is not one of "a","b"`, "weights[1]: 0.1 is below the minimum 0.5"}},
		{name: "another spelling of a known field", schema: open, args: `{` + ok + `,"QUERY":"x"}`,
			want: []string{`QUERY: unknown field; did you mean "query"?`}},
		{name: "enum numbers compare exactly", schema: open, args: `{` + ok + `,"exact":9007199254740993}`,
			want: []string{"exact: 9007199254740993 is not one of 9007199254740992"}},
		{name: "enum number past any float", schema: open, args: `{` + ok + `,"exact":1e999999999}`,
			want: []string{"exact: 1e999999999 is not one of 9007199254740992"}},
		{name: "nested objects are checked", schema: open, args: `{` + ok + `,"sources":[{"rank":3}]}`,
			want: []string{"sources[0].url: required", "sources[0].rank: 3 is not one of 1,2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.schema.Validate(tc.args)
			if len(tc.want) == 0 {
				if err != nil {
					t.Errorf("Validate = %v; want valid", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate = nil; want %q", tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate = %q; want it to mention %q", err, want)
				}
			}
			if got := strings.Count(err.Error(), "\n") + 1; got != len(tc.want) {
				t.Errorf("Validate reported %d problems, want %d: %q", got, len(tc.want), err)
			}
		})
	}
}

// TestValidate_BoundsCompareExactly: 2^63+1 rounds to 2^63 as a float64,
// which must not let it pass a maximum of 2^63. A number field compares as
// the float64 it becomes, so 0.3 is not above a maximum of 0.3.
func TestValidate_BoundsCompareExactly(t *testing.T) {
	schema, err := SchemaFor[struct {
		N uint64  `json:"n,omitempty" min:"9007199254740992" max:"9223372036854775808"`
		F float64 `json:"f,omitempty" max:"0.3"`
	}]()
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	for args, valid := range map[string]bool{
		`{"n":9223372036854775808}`: true,
		`{"n":9223372036854775809}`: false,
		`{"n":9007199254740992}`:    true,
		`{"n":9007199254740991}`:    false,
		`{"f":0.3}`:                 true,
		`{"f":0.30000000000000004}`: false,
		`{"f":1e999}`:               false,
	} {
		if err := schema.Validate(args); (err == nil) != valid {
			t.Errorf("Validate(%s) = %v; want valid: %v", args, err, valid)
		}
	}
}

// TestValidate_NumberEnumComparesByValue: 1 and 1.0 are one number.
func TestValidate_NumberEnumComparesByValue(t *testing.T) {
	schema := FunctionParams{Type: "object", Properties: map[string]FunctionParam{
		"x": {Type: "number", Enum: []any{1, 0.5}},
	}}
	for args, valid := range map[string]bool{`{"x":1.0}`: true, `{"x":5e-1}`: true, `{"x":0.51}`: false} {
		if err := schema.Validate(args); (err == nil) != valid {
			t.Errorf("Validate(%s) = %v; want valid: %v", args, err, valid)
		}
	}
}

// TestValidate_UntypedSchemaAcceptsAnything: a schema fragment with no
// type, as a lossy conversion leaves behind, must not reject every value.
func TestValidate_UntypedSchemaAcceptsAnything(t *testing.T) {
	schema := FunctionParams{Type: "object", Properties: map[string]FunctionParam{"x": {}}, Required: []string{"x"}}
	for _, args := range []string{`{"x":[1,"two"]}`, `{"x":null}`} {
		if err := schema.Validate(args); err != nil {
			t.Errorf("Validate(%s) = %v; want valid", args, err)
		}
	}
}
