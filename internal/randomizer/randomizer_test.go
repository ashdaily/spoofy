package randomizer

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"gopkg.in/yaml.v3"

	"github.com/ashdaily/spoofy/internal/openapi"
)

func schemaFromYAML(t *testing.T, body string) *openapi3.Schema {
	t.Helper()
	var s openapi3.Schema
	if err := s.UnmarshalJSON(yamlToJSON(t, body)); err != nil {
		t.Fatalf("parsing schema: %v", err)
	}
	return &s
}

// yamlToJSON keeps the test fixtures readable as YAML while feeding
// kin-openapi the JSON it unmarshals from.
func yamlToJSON(t *testing.T, body string) []byte {
	t.Helper()
	var v any
	if err := yamlUnmarshal([]byte(body), &v); err != nil {
		t.Fatalf("parsing test YAML: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-encoding test YAML: %v", err)
	}
	return out
}

// The preference order is the core contract of this package: anything the spec
// author wrote by hand must beat anything we invent.
func TestSpecProvidedValuesWinOverGeneration(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   any
	}{
		{
			name:   "example beats everything",
			schema: "{type: string, format: uuid, example: mango, enum: [a, b], default: d}",
			want:   "mango",
		},
		{
			name:   "default is used when there is no example",
			schema: "{type: string, default: fallback, enum: [a, b]}",
			want:   "fallback",
		},
		{
			name:   "const is honoured",
			schema: "{type: string, const: fixed}",
			want:   "fixed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(1)
			if got := r.Value(schemaFromYAML(t, tc.schema)); got != tc.want {
				t.Errorf("Value() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnumValuesAreAlwaysMembers(t *testing.T) {
	r := New(1)
	schema := schemaFromYAML(t, "{type: string, enum: [available, pending, sold]}")
	allowed := map[any]bool{"available": true, "pending": true, "sold": true}

	seen := make(map[any]bool)
	for i := 0; i < 200; i++ {
		got := r.Value(schema)
		if !allowed[got] {
			t.Fatalf("generated %v, which is not in the enum", got)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("only ever generated %v; the enum is not being sampled", seen)
	}
}

func TestFormatsProduceParseableValues(t *testing.T) {
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	ipv4Re := regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$`)

	tests := []struct {
		format string
		check  func(t *testing.T, got string)
	}{
		{"uuid", func(t *testing.T, got string) {
			if !uuidRe.MatchString(got) {
				t.Errorf("%q is not a valid v4 UUID", got)
			}
		}},
		{"email", func(t *testing.T, got string) {
			if !strings.Contains(got, "@") || !strings.Contains(got, ".") {
				t.Errorf("%q does not look like an email", got)
			}
		}},
		{"date-time", func(t *testing.T, got string) {
			if _, err := time.Parse(time.RFC3339, got); err != nil {
				t.Errorf("%q is not RFC3339: %v", got, err)
			}
		}},
		{"date", func(t *testing.T, got string) {
			if _, err := time.Parse("2006-01-02", got); err != nil {
				t.Errorf("%q is not a date: %v", got, err)
			}
		}},
		{"ipv4", func(t *testing.T, got string) {
			if !ipv4Re.MatchString(got) {
				t.Errorf("%q is not an IPv4 address", got)
			}
		}},
		{"uri", func(t *testing.T, got string) {
			if !strings.HasPrefix(got, "https://") {
				t.Errorf("%q is not a URI", got)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.format, func(t *testing.T) {
			r := New(9)
			for i := 0; i < 50; i++ {
				got, ok := r.Value(schemaFromYAML(t, "{type: string, format: "+tc.format+"}")).(string)
				if !ok {
					t.Fatalf("format %s did not produce a string", tc.format)
				}
				tc.check(t, got)
			}
		})
	}
}

func TestNumericConstraints(t *testing.T) {
	r := New(4)

	t.Run("integer bounds", func(t *testing.T) {
		schema := schemaFromYAML(t, "{type: integer, minimum: 10, maximum: 20}")
		for i := 0; i < 500; i++ {
			got, ok := r.Value(schema).(int64)
			if !ok {
				t.Fatalf("expected int64, got %T", r.Value(schema))
			}
			if got < 10 || got > 20 {
				t.Fatalf("generated %d, outside [10, 20]", got)
			}
		}
	})

	t.Run("number bounds", func(t *testing.T) {
		schema := schemaFromYAML(t, "{type: number, minimum: 0.1, maximum: 90}")
		for i := 0; i < 500; i++ {
			got := r.Value(schema).(float64)
			if got < 0.1 || got > 90 {
				t.Fatalf("generated %v, outside [0.1, 90]", got)
			}
		}
	})

	t.Run("multipleOf", func(t *testing.T) {
		schema := schemaFromYAML(t, "{type: integer, minimum: 0, maximum: 100, multipleOf: 5}")
		for i := 0; i < 200; i++ {
			got := r.Value(schema).(int64)
			if got%5 != 0 {
				t.Fatalf("generated %d, not a multiple of 5", got)
			}
		}
	})
}

func TestStringLengthConstraints(t *testing.T) {
	r := New(5)
	schema := schemaFromYAML(t, "{type: string, minLength: 5, maxLength: 12}")

	for i := 0; i < 500; i++ {
		got := r.Value(schema).(string)
		if len(got) < 5 || len(got) > 12 {
			t.Fatalf("generated %q of length %d, outside [5, 12]", got, len(got))
		}
	}
}

func TestArrayItemCountConstraints(t *testing.T) {
	r := New(6)
	schema := schemaFromYAML(t, "{type: array, minItems: 2, maxItems: 4, items: {type: string}}")

	for i := 0; i < 300; i++ {
		got := r.Value(schema).([]any)
		if len(got) < 2 || len(got) > 4 {
			t.Fatalf("generated %d items, outside [2, 4]", len(got))
		}
	}
}

func TestRequiredPropertiesAreAlwaysPresent(t *testing.T) {
	r := New(8)
	schema := schemaFromYAML(t, `
type: object
required: [id, name]
properties:
  id: {type: string}
  name: {type: string}
  nickname: {type: string}
`)

	for i := 0; i < 300; i++ {
		got := r.Value(schema).(map[string]any)
		for _, key := range []string{"id", "name"} {
			if _, ok := got[key]; !ok {
				t.Fatalf("required property %q missing from %v", key, got)
			}
		}
	}
}

// Optional fields should appear sometimes and be omitted sometimes; always
// sending an identically-shaped body under-exercises the API.
func TestOptionalPropertiesVary(t *testing.T) {
	r := New(10)
	schema := schemaFromYAML(t, `
type: object
required: [id]
properties:
  id: {type: string}
  nickname: {type: string}
`)

	var present, absent int
	for i := 0; i < 300; i++ {
		got := r.Value(schema).(map[string]any)
		if _, ok := got["nickname"]; ok {
			present++
		} else {
			absent++
		}
	}
	if present == 0 || absent == 0 {
		t.Errorf("optional field present %d times, absent %d; expected both", present, absent)
	}
}

// Read-only fields are server-owned. Sending them invites a 400 on APIs that
// validate strictly.
func TestReadOnlyPropertiesAreOmitted(t *testing.T) {
	r := New(12)
	schema := schemaFromYAML(t, `
type: object
required: [name]
properties:
  name: {type: string}
  createdAt: {type: string, readOnly: true}
`)

	for i := 0; i < 200; i++ {
		got := r.Value(schema).(map[string]any)
		if _, ok := got["createdAt"]; ok {
			t.Fatal("read-only property was included in a generated body")
		}
	}
}

// A self-referential schema is common and would recurse forever without a
// depth limit, hours into a run rather than immediately.
func TestCyclicSchemaTerminates(t *testing.T) {
	node := &openapi3.Schema{
		Type:       &openapi3.Types{"object"},
		Required:   []string{"id"},
		Properties: openapi3.Schemas{},
	}
	node.Properties["id"] = &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}}
	// child points back at node, forming a cycle.
	node.Properties["child"] = &openapi3.SchemaRef{Value: node}

	done := make(chan any, 1)
	go func() { done <- New(1).Value(node) }()

	select {
	case got := <-done:
		if _, ok := got.(map[string]any); !ok {
			t.Fatalf("expected an object, got %T", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not terminate on a cyclic schema")
	}
}

func TestSameSeedProducesSameValues(t *testing.T) {
	schema := schemaFromYAML(t, `
type: object
required: [id, name, tags]
properties:
  id: {type: string, format: uuid}
  name: {type: string}
  tags: {type: array, items: {type: string}}
  score: {type: integer}
`)

	first, _ := json.Marshal(New(1234).Value(schema))
	second, _ := json.Marshal(New(1234).Value(schema))
	if string(first) != string(second) {
		t.Errorf("same seed diverged:\n  %s\n  %s", first, second)
	}

	third, _ := json.Marshal(New(5678).Value(schema))
	if string(first) == string(third) {
		t.Error("different seeds produced identical output")
	}
}

// The end-to-end assertion that matters: values generated from a real spec must
// validate against that same spec. If this passes, generated traffic is not
// 400 soup.
func TestGeneratedValuesValidateAgainstTheirSchema(t *testing.T) {
	spec, err := openapi.Load(context.Background(), "../openapi/testdata/petstore.yaml")
	if err != nil {
		t.Fatalf("loading spec: %v", err)
	}

	var body *openapi3.Schema
	for _, op := range spec.Operations {
		if op.Method == "POST" && op.Path == "/pets" {
			media := op.RequestBody.Content.Get("application/json")
			body = media.Schema.Value
		}
	}
	if body == nil {
		t.Fatal("could not find the POST /pets request schema")
	}

	r := New(99)
	for i := 0; i < 300; i++ {
		value := r.Value(body)

		// Round-trip through JSON so validation sees exactly what would go on
		// the wire (int64 becomes float64, and so on).
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshalling generated value: %v", err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshalling generated value: %v", err)
		}

		if err := body.VisitJSON(decoded); err != nil {
			t.Fatalf("generated body does not satisfy its own schema: %v\n  body: %s", err, encoded)
		}
	}
}

func yamlUnmarshal(in []byte, out any) error { return yaml.Unmarshal(in, out) }
