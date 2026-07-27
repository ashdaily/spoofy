// Package randomizer turns an OpenAPI schema into a plausible concrete value.
//
// "Plausible" is the whole point. A generator that emits syntactically valid
// but semantically absurd data produces an environment full of 400s and 404s,
// which is worse than no traffic at all: the dashboards look busy and prove
// nothing. So the strategy is a strict preference order, from what the spec
// author actually wrote down to what we had to invent:
//
//	example -> examples -> default -> const -> enum -> format -> pattern -> type
//
// Spec-provided examples come first because they are real values from someone
// who knows the API. Free realism, and the cheapest defence against 404 soup.
package randomizer

import (
	"encoding/base64"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// defaultMaxDepth bounds recursion. Self-referential schemas (a Node with
// children of type Node) are common and would otherwise recurse until the
// stack gives out — in a daemon, hours after it started.
const defaultMaxDepth = 6

// optionalFieldChance is how often an optional property is included. Always
// including them makes every payload identical in shape; never including them
// under-exercises the API. Two-in-three produces varied but substantial bodies.
const optionalFieldChance = 0.66

// Randomizer generates values from schemas.
//
// It is NOT safe for concurrent use, matching math/rand.Rand. Give each worker
// its own instance seeded from a common base — that keeps runs reproducible
// per worker, which a shared mutex-guarded instance could not.
type Randomizer struct {
	rng      *rand.Rand
	maxDepth int
}

// New returns a Randomizer. A zero seed means seed from the clock.
func New(seed int64) *Randomizer {
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &Randomizer{rng: rand.New(rand.NewSource(seed)), maxDepth: defaultMaxDepth}
}

// Value generates a value for the schema. It returns nil only when the schema
// carries no usable type information at all.
func (r *Randomizer) Value(schema *openapi3.Schema) any {
	return r.value(schema, 0)
}

// Bool returns a coin flip from the same stream, so callers deciding whether to
// include an optional field stay inside the seeded sequence.
func (r *Randomizer) Bool() bool { return r.rng.Intn(2) == 0 }

// Chance reports true with probability p, drawn from the same stream.
func (r *Randomizer) Chance(p float64) bool { return r.rng.Float64() < p }

func (r *Randomizer) value(s *openapi3.Schema, depth int) any {
	if s == nil {
		return nil
	}

	// Anything the spec author wrote by hand beats anything we invent.
	if s.Example != nil {
		return s.Example
	}
	if len(s.Examples) > 0 {
		return s.Examples[r.rng.Intn(len(s.Examples))]
	}
	if s.Default != nil {
		return s.Default
	}
	if s.Const != nil {
		return s.Const
	}
	if len(s.Enum) > 0 {
		return s.Enum[r.rng.Intn(len(s.Enum))]
	}

	if depth > r.maxDepth {
		return r.shallowValue(s)
	}

	// Composition. allOf is a merge; oneOf/anyOf are a choice.
	if len(s.AllOf) > 0 {
		return r.allOf(s, depth)
	}
	if choice := r.pickBranch(s.OneOf, s.AnyOf); choice != nil {
		return r.value(choice, depth+1)
	}

	switch {
	case hasType(s, "object"), s.Type == nil && len(s.Properties) > 0:
		return r.object(s, depth)
	case hasType(s, "array"):
		return r.array(s, depth)
	case hasType(s, "string"):
		return r.String(s)
	case hasType(s, "integer"):
		return r.Integer(s)
	case hasType(s, "number"):
		return r.Number(s)
	case hasType(s, "boolean"):
		return r.rng.Intn(2) == 0
	}

	// A schema with no type and no properties is legal and means "anything".
	return r.word()
}

// shallowValue is the depth-limit fallback: a valid value of the right type
// that does not recurse.
func (r *Randomizer) shallowValue(s *openapi3.Schema) any {
	switch {
	case hasType(s, "string"):
		return r.String(s)
	case hasType(s, "integer"):
		return r.Integer(s)
	case hasType(s, "number"):
		return r.Number(s)
	case hasType(s, "boolean"):
		return true
	case hasType(s, "array"):
		return []any{}
	default:
		return map[string]any{}
	}
}

func (r *Randomizer) pickBranch(oneOf, anyOf openapi3.SchemaRefs) *openapi3.Schema {
	refs := oneOf
	if len(refs) == 0 {
		refs = anyOf
	}
	if len(refs) == 0 {
		return nil
	}
	ref := refs[r.rng.Intn(len(refs))]
	if ref == nil {
		return nil
	}
	return ref.Value
}

// allOf composes the branches into one object. Non-object branches are rare in
// practice; if one appears, its value is returned directly rather than dropped.
func (r *Randomizer) allOf(s *openapi3.Schema, depth int) any {
	merged := make(map[string]any)
	for _, ref := range s.AllOf {
		if ref == nil || ref.Value == nil {
			continue
		}
		part := r.value(ref.Value, depth+1)
		m, ok := part.(map[string]any)
		if !ok {
			return part
		}
		for k, v := range m {
			merged[k] = v
		}
	}
	// Properties declared alongside allOf still apply.
	for _, name := range sortedProps(s.Properties) {
		if ref := s.Properties[name]; ref != nil && ref.Value != nil {
			merged[name] = r.value(ref.Value, depth+1)
		}
	}
	return merged
}

func (r *Randomizer) object(s *openapi3.Schema, depth int) any {
	out := make(map[string]any, len(s.Properties))

	required := make(map[string]bool, len(s.Required))
	for _, name := range s.Required {
		required[name] = true
	}

	// Sorted, not map order. Go randomises map iteration, which would consume
	// the rng in a different sequence on every run and quietly break --seed
	// reproducibility — the kind of bug that only shows up when you try to
	// reproduce an incident and cannot.
	for _, name := range sortedProps(s.Properties) {
		ref := s.Properties[name]
		if ref == nil || ref.Value == nil {
			continue
		}
		// Read-only fields are server-owned; sending them invites a 400.
		if ref.Value.ReadOnly {
			continue
		}
		if !required[name] && r.rng.Float64() > optionalFieldChance {
			continue
		}
		out[name] = r.value(ref.Value, depth+1)
	}

	// Required properties must be present even if the loop above skipped them
	// (for instance a required read-only field, which is a spec bug we should
	// not turn into a malformed request). s.Required is a slice, so this is
	// already deterministic.
	for _, name := range s.Required {
		if _, ok := out[name]; ok {
			continue
		}
		if ref := s.Properties[name]; ref != nil && ref.Value != nil {
			out[name] = r.value(ref.Value, depth+1)
		}
	}

	return out
}

// sortedProps gives deterministic iteration over a schema's properties.
func sortedProps(props openapi3.Schemas) []string {
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Randomizer) array(s *openapi3.Schema, depth int) any {
	minItems := int(s.MinItems)
	maxItems := minItems + 2
	if s.MaxItems != nil {
		maxItems = int(*s.MaxItems)
	}
	if maxItems < minItems {
		maxItems = minItems
	}
	if maxItems == 0 {
		maxItems = 1
	}

	n := minItems
	if maxItems > minItems {
		n = minItems + r.rng.Intn(maxItems-minItems+1)
	}
	if n == 0 && minItems == 0 {
		n = 1 // an always-empty array exercises nothing
	}

	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		if s.Items != nil && s.Items.Value != nil {
			out = append(out, r.value(s.Items.Value, depth+1))
		} else {
			out = append(out, r.word())
		}
	}
	return out
}

// String generates a string honouring format, then pattern, then length bounds.
func (r *Randomizer) String(s *openapi3.Schema) string {
	if v, ok := r.byFormat(s.Format); ok {
		return v
	}
	if s.Pattern != "" {
		if v, ok := GeneratePattern(r.rng, s.Pattern); ok {
			return v
		}
		// Falling through on an unsupported pattern is deliberate: a plausible
		// string that may fail validation beats an empty one that certainly will.
	}

	minLen := int(s.MinLength)
	maxLen := minLen + 12
	if s.MaxLength != nil {
		maxLen = int(*s.MaxLength)
	}
	if maxLen < minLen {
		maxLen = minLen
	}
	if maxLen == 0 {
		return ""
	}

	var b strings.Builder
	for b.Len() < minLen || (b.Len() < 3 && maxLen >= 3) {
		if b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteString(r.word())
	}
	if b.Len() == 0 {
		b.WriteString(r.word())
	}

	out := b.String()
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	for len(out) < minLen {
		out += "x"
	}
	return out
}

func (r *Randomizer) byFormat(format string) (string, bool) {
	switch strings.ToLower(format) {
	case "uuid":
		return r.uuid(), true
	case "email", "idn-email":
		return fmt.Sprintf("%s.%s@example.com", r.word(), r.word()), true
	case "date-time":
		return r.recentTime().Format(time.RFC3339), true
	case "date":
		return r.recentTime().Format("2006-01-02"), true
	case "time":
		return r.recentTime().Format("15:04:05"), true
	case "duration":
		return fmt.Sprintf("PT%dM", 1+r.rng.Intn(120)), true
	case "hostname", "idn-hostname":
		return fmt.Sprintf("%s-%d.example.com", r.word(), r.rng.Intn(100)), true
	case "ipv4":
		return fmt.Sprintf("10.%d.%d.%d", r.rng.Intn(256), r.rng.Intn(256), 1+r.rng.Intn(254)), true
	case "ipv6":
		return fmt.Sprintf("2001:db8::%x", r.rng.Intn(0xffff)), true
	case "uri", "url", "uri-reference":
		return fmt.Sprintf("https://example.com/%s/%d", r.word(), r.rng.Intn(1000)), true
	case "byte":
		buf := make([]byte, 12)
		for i := range buf {
			buf[i] = byte(r.rng.Intn(256))
		}
		return base64.StdEncoding.EncodeToString(buf), true
	case "password":
		return fmt.Sprintf("%s-%s-%d", r.word(), r.word(), r.rng.Intn(10000)), true
	}
	return "", false
}

// Integer generates an integer within the schema's bounds.
func (r *Randomizer) Integer(s *openapi3.Schema) int64 {
	lo, hi := int64(1), int64(1000)
	if s.Min != nil {
		lo = int64(math.Ceil(*s.Min))
	}
	if s.Max != nil {
		hi = int64(math.Floor(*s.Max))
	}
	if hi < lo {
		hi = lo
	}

	v := lo
	if hi > lo {
		v = lo + r.rng.Int63n(hi-lo+1)
	}

	if s.MultipleOf != nil && *s.MultipleOf > 0 {
		step := int64(*s.MultipleOf)
		if step > 0 {
			v -= v % step
			if v < lo {
				v += step
			}
		}
	}
	return v
}

// Number generates a float within the schema's bounds, rounded to two decimal
// places because most real APIs carry money or measurements, not raw float64s.
func (r *Randomizer) Number(s *openapi3.Schema) float64 {
	lo, hi := 0.0, 1000.0
	if s.Min != nil {
		lo = *s.Min
	}
	if s.Max != nil {
		hi = *s.Max
	}
	if hi < lo {
		hi = lo
	}
	v := lo + r.rng.Float64()*(hi-lo)
	return math.Round(v*100) / 100
}

func (r *Randomizer) uuid() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(r.rng.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (r *Randomizer) recentTime() time.Time {
	// A fixed anchor keeps seeded runs reproducible; drifting off time.Now()
	// would make golden-value tests flaky.
	anchor := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	return anchor.Add(time.Duration(r.rng.Int63n(int64(365 * 24 * time.Hour))))
}

// words are short, pronounceable, and neutral. Payloads full of "aXk2Ppq" make
// a staging environment look broken to anyone glancing at a log.
var words = []string{
	"amber", "basil", "cedar", "delta", "ember", "fable", "grove", "harbor",
	"indigo", "juniper", "kestrel", "lumen", "marble", "nimbus", "onyx", "pebble",
	"quartz", "ridge", "summit", "topaz", "umber", "verde", "willow", "zephyr",
}

func (r *Randomizer) word() string { return words[r.rng.Intn(len(words))] }

func hasType(s *openapi3.Schema, t string) bool {
	return s.Type != nil && s.Type.Includes(t)
}
