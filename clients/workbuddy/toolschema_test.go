package workbuddy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"client2api/internal/core"
)

// normalizeJSON decodes a schema the way wireToolParameters does (UseNumber,
// so bounds and defaults keep their exact text), normalizes it and returns the
// canonical JSON.
func normalizeJSON(t *testing.T, in string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(in))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", in, err)
	}
	out, err := json.Marshal(normalizeToolSchema(v))
	if err != nil {
		t.Fatalf("marshal normalized %s: %v", in, err)
	}
	return string(out)
}

// requireSchema compares two schemas structurally, so key order and whitespace
// never matter.
func requireSchema(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v (%s)", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("schema mismatch\n got: %s\nwant: %s", got, want)
	}
}

// Every construct below is valid JSON Schema that WorkBuddy's tool validator
// answers with business code 11129 ("invalid function call parameters").  Each
// case pins the equivalent shape it accepts instead; the live checks behind
// these cases were run against workbuddy/global:deepseek-v4.1-flash.
func TestNormalizeToolSchema(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty schema becomes the canonical empty parameters",
			in:   `{}`,
			want: `{"type":"object","properties":{}}`,
		},
		{
			name: "root properties without type gain one",
			in:   `{"properties":{"a":{"type":"string"}},"required":["a"]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		},
		{
			name: "root array without type gains one",
			in:   `{"items":{"type":"string"}}`,
			want: `{"type":"array","items":{"type":"string"}}`,
		},
		{
			name: "tuple items with two branches become anyOf",
			in:   `{"type":"object","properties":{"pair":{"type":"array","items":[{"type":"string"},{"type":"number"}]}}}`,
			want: `{"type":"object","properties":{"pair":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"number"}]}}}}`,
		},
		{
			name: "single-branch tuple collapses to that schema",
			in:   `{"type":"object","properties":{"a":{"type":"array","items":[{"type":"string"}]}}}`,
			want: `{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"}}}}`,
		},
		{
			name: "empty tuple is dropped",
			in:   `{"type":"object","properties":{"a":{"type":"array","items":[]}}}`,
			want: `{"type":"object","properties":{"a":{"type":"array"}}}`,
		},
		{
			name: "null items is dropped",
			in:   `{"type":"object","properties":{"a":{"type":"array","items":null}}}`,
			want: `{"type":"object","properties":{"a":{"type":"array"}}}`,
		},
		{
			name: "items object without type gains object",
			in:   `{"type":"object","properties":{"a":{"type":"array","items":{"properties":{"b":{"type":"string"}}}}}}`,
			want: `{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","properties":{"b":{"type":"string"}}}}}}`,
		},
		{
			name: "root anyOf keeps its first object branch",
			in:   `{"anyOf":[{"type":"string"},{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		},
		{
			name: "root oneOf keeps its first object branch",
			in:   `{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "root allOf merges every branch",
			in:   `{"type":"object","allOf":[{"properties":{"a":{"type":"string"}}},{"properties":{"b":{"type":"number"}},"required":["b"]}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["b"]}`,
		},
		{
			name: "root ref is inlined and the now-unused defs are pruned",
			in:   `{"$ref":"#/$defs/P","$defs":{"P":{"type":"object","properties":{"a":{"type":"string"}}}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "a nested tuple inside a still-referenced def is rewritten and the def survives",
			in:   `{"type":"object","properties":{"t":{"$ref":"#/$defs/T"}},"$defs":{"T":{"type":"array","items":[{"type":"string"},{"type":"number"}]}}}`,
			want: `{"type":"object","properties":{"t":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"number"}]}}}}`,
		},
		{
			name: "nested refs are inlined wherever they appear",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/$defs/P"},"b":{"$ref":"#/$defs/P"}},"$defs":{"P":{"type":"object","properties":{"x":{"type":"string"}}}}}`,
			want: `{"type":"object","properties":{"a":{"type":"object","properties":{"x":{"type":"string"}}},"b":{"type":"object","properties":{"x":{"type":"string"}}}}}`,
		},
		{
			name: "a self-referential def is cut rather than expanded forever",
			in:   `{"type":"object","properties":{"n":{"$ref":"#/$defs/Node"}},"$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}}}`,
			want: `{"type":"object","properties":{"n":{"type":"object","properties":{}}}}`,
		},
		{
			name: "nullable type array collapses to the non-null type",
			in:   `{"type":"object","properties":{"a":{"type":["string","null"]}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "an all-null type array is dropped with its now-empty property",
			in:   `{"type":"object","properties":{"a":{"type":["null"]}}}`,
			want: `{"type":"object","properties":{}}`,
		},
		{
			name: "a union type becomes anyOf",
			in:   `{"type":"object","properties":{"a":{"type":["string","number"]}}}`,
			want: `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"number"}]}}}`,
		},
		{
			name: "a type array that also declares properties stays an object",
			in:   `{"type":"object","properties":{"a":{"type":["object","null"],"properties":{"b":{"type":"string"}}}}}`,
			want: `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"string"}}}}}`,
		},
		{
			name: "const becomes a one-value enum",
			in:   `{"type":"object","properties":{"kind":{"type":"string","const":"city"}}}`,
			want: `{"type":"object","properties":{"kind":{"type":"string","enum":["city"]}}}`,
		},
		{
			name: "a const next to an enum is dropped rather than merged",
			in:   `{"type":"object","properties":{"k":{"type":"string","const":"a","enum":["a","b"]}}}`,
			want: `{"type":"object","properties":{"k":{"type":"string","enum":["a","b"]}}}`,
		},
		{
			name: "nullable and schema metadata keywords are dropped",
			in:   `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"a":{"type":"string","nullable":true,"$comment":"x"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "a required name with no declared property is dropped",
			in:   `{"type":"object","properties":{"a":{"type":"string"},"b":null},"required":["a","b"]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		},
		{
			name: "an all-dropped required list disappears",
			in:   `{"type":"object","properties":{"a":{"type":"string"},"b":null},"required":["b"]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "unresolvable root ref is dropped",
			in:   `{"$ref":"https://example.test/schema.json","type":"object","properties":{}}`,
			want: `{"type":"object","properties":{}}`,
		},
		{
			name: "boolean exclusiveMinimum true folds into the numeric form",
			in:   `{"type":"object","properties":{"a":{"type":"number","minimum":0,"exclusiveMinimum":true}}}`,
			want: `{"type":"object","properties":{"a":{"type":"number","exclusiveMinimum":0}}}`,
		},
		{
			name: "boolean exclusiveMaximum false is dropped",
			in:   `{"type":"object","properties":{"a":{"type":"number","maximum":10,"exclusiveMaximum":false}}}`,
			want: `{"type":"object","properties":{"a":{"type":"number","maximum":10}}}`,
		},
		{
			name: "numeric exclusiveMinimum is kept",
			in:   `{"type":"object","properties":{"a":{"type":"number","exclusiveMinimum":5}}}`,
			want: `{"type":"object","properties":{"a":{"type":"number","exclusiveMinimum":5}}}`,
		},
		{
			name: "string required becomes an array",
			in:   `{"type":"object","properties":{"a":{"type":"string"}},"required":"a"}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		},
		{
			name: "null property schema is dropped",
			in:   `{"type":"object","properties":{"a":{"type":"string"},"b":null}}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "tuple nested inside an items schema is rewritten too",
			in:   `{"type":"object","properties":{"a":{"type":"array","items":{"type":"array","items":[{"type":"string"}]}}}}`,
			want: `{"type":"object","properties":{"a":{"type":"array","items":{"type":"array","items":{"type":"string"}}}}}`,
		},
		{
			name: "the old defs-only tuple case now prunes the unreferenced def",
			in:   `{"type":"object","$defs":{"T":{"type":"array","items":[{"type":"string"},{"type":"number"}]}},"properties":{}}`,
			want: `{"type":"object","properties":{}}`,
		},
		{
			name: "value constraints survive byte-for-byte",
			in:   `{"type":"object","properties":{"n":{"type":"integer","minimum":1.5,"default":9007199254740993}}}`,
			want: `{"type":"object","properties":{"n":{"type":"integer","minimum":1.5,"default":9007199254740993}}}`,
		},
		{
			name: "a plain object schema is untouched",
			in:   `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`,
			want: `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireSchema(t, normalizeJSON(t, tc.in), tc.want)
		})
	}
}

// A schema that is not a JSON object cannot be sent at all; the canonical empty
// parameters keep the tool usable.
func TestNormalizeToolSchemaNonObject(t *testing.T) {
	for _, in := range []string{`[]`, `"oops"`, `null`, `true`} {
		got := normalizeJSON(t, in)
		requireSchema(t, got, `{"type":"object","properties":{}}`)
	}
}

// wireTools must never forward unsupported constructs, and must always publish
// a parameters object so a tool without a schema stays callable.
func TestWireToolsNormalizesParameters(t *testing.T) {
	raw, err := buildWireBody(&core.ChatRequest{
		Model: "deepseek-v4.1-flash",
		Messages: []core.Message{
			{Role: "user", Content: "go"},
		},
		Tools: []core.Tool{
			{Name: "tuple", Parameters: []byte(`{"properties":{"a":{"type":"array","items":[{"type":"string"}]}}}`)},
			{Name: "bare"},
		},
	})
	if err != nil {
		t.Fatalf("buildWireBody: %v", err)
	}
	var body struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(body.Tools))
	}
	requireSchema(t, string(body.Tools[0].Function.Parameters),
		`{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"}}}}`)
	requireSchema(t, string(body.Tools[1].Function.Parameters),
		`{"type":"object","properties":{}}`)
}
