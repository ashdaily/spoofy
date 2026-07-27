// Package openapi loads an OpenAPI document and flattens it into the small,
// concrete list of operations Spoofy actually needs.
//
// The rest of the codebase never touches the raw document. Everything
// downstream works from []*Operation, which keeps spec-shaped complexity —
// $ref resolution, parameter inheritance, 3.0-vs-3.1 differences — contained
// in one package instead of leaking into request generation.
package openapi

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Operation is one callable endpoint: a method, a templated path, and
// everything needed to build a request for it.
type Operation struct {
	// ID is the operationId when the spec provides one, otherwise a synthesised
	// stable identifier. Used for logging and metrics labels.
	ID string
	// Method is the uppercase HTTP method.
	Method string
	// Path is the templated path exactly as written in the spec, e.g.
	// "/orders/{orderId}". Metrics label on this, never the concrete URL, or
	// Prometheus cardinality explodes over a long run.
	Path string
	// Summary is the spec's human description, shown in dry-run output.
	Summary string
	// Params is the merged parameter set: path-item-level parameters plus
	// operation-level ones, with operation-level winning on conflict.
	Params []*openapi3.Parameter
	// RequestBody is nil for operations that take no body.
	RequestBody *openapi3.RequestBody
	// Deprecated mirrors the spec flag.
	Deprecated bool
}

// String renders "GET /orders/{id}", the form used in logs and dry-run output.
func (o *Operation) String() string { return o.Method + " " + o.Path }

// Spec is a flattened OpenAPI document.
type Spec struct {
	Title      string
	Version    string
	Servers    []string
	Operations []*Operation
}

// Load reads an OpenAPI document from a file path or an http(s) URL.
//
// External $ref resolution is deliberately left disabled. kin-openapi will
// otherwise follow references out of the document, which on a spec you did not
// write means local file reads (`$ref: "/etc/passwd"`) and SSRF against
// metadata endpoints. Specs are frequently fetched from a running service, so
// treating them as untrusted input is the correct default.
func Load(ctx context.Context, source string) (*Spec, error) {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	loader.IsExternalRefsAllowed = false

	var (
		doc *openapi3.T
		err error
	)
	if isRemote(source) {
		u, perr := url.Parse(source)
		if perr != nil {
			return nil, fmt.Errorf("spec URL %q is not valid: %w", source, perr)
		}
		doc, err = loader.LoadFromURI(u)
	} else {
		if _, serr := os.Stat(source); serr != nil {
			return nil, fmt.Errorf("spec %q not found: %w", source, serr)
		}
		doc, err = loader.LoadFromFile(source)
	}
	if err != nil {
		return nil, fmt.Errorf("loading spec %q: %w", source, err)
	}

	return FromDocument(doc)
}

// FromDocument flattens an already-parsed document. Exported so tests can build
// documents in memory without touching the filesystem.
func FromDocument(doc *openapi3.T) (*Spec, error) {
	if doc == nil {
		return nil, fmt.Errorf("spec is empty")
	}

	spec := &Spec{}
	if doc.Info != nil {
		spec.Title = doc.Info.Title
		spec.Version = doc.Info.Version
	}
	for _, s := range doc.Servers {
		if s != nil && s.URL != "" {
			spec.Servers = append(spec.Servers, s.URL)
		}
	}

	if doc.Paths == nil {
		return nil, fmt.Errorf("spec declares no paths")
	}

	for _, path := range sortedKeys(doc.Paths.Map()) {
		item := doc.Paths.Value(path)
		if item == nil {
			continue
		}
		for _, method := range sortedKeys(item.Operations()) {
			op := item.Operations()[method]
			if op == nil {
				continue
			}
			spec.Operations = append(spec.Operations, buildOperation(path, method, item, op))
		}
	}

	if len(spec.Operations) == 0 {
		return nil, fmt.Errorf("spec declares no operations")
	}

	// Stable ordering keeps dry-run output and seeded runs reproducible.
	sort.Slice(spec.Operations, func(i, j int) bool {
		if spec.Operations[i].Path != spec.Operations[j].Path {
			return spec.Operations[i].Path < spec.Operations[j].Path
		}
		return spec.Operations[i].Method < spec.Operations[j].Method
	})

	return spec, nil
}

func buildOperation(path, method string, item *openapi3.PathItem, op *openapi3.Operation) *Operation {
	out := &Operation{
		ID:         op.OperationID,
		Method:     strings.ToUpper(method),
		Path:       path,
		Summary:    op.Summary,
		Params:     mergeParams(item.Parameters, op.Parameters),
		Deprecated: op.Deprecated,
	}
	if out.ID == "" {
		out.ID = synthesiseID(out.Method, path)
	}
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		out.RequestBody = op.RequestBody.Value
	}
	return out
}

// mergeParams implements the OpenAPI inheritance rule: parameters declared on
// the path item apply to every operation under it, and an operation-level
// parameter with the same name+location replaces the inherited one.
//
// Getting this wrong is a quiet bug — the operation still builds, it just omits
// a required path parameter and every request 404s — so it is worth doing
// explicitly rather than by concatenation.
func mergeParams(pathLevel, opLevel openapi3.Parameters) []*openapi3.Parameter {
	type key struct{ name, in string }

	merged := make(map[key]*openapi3.Parameter)
	order := make([]key, 0, len(pathLevel)+len(opLevel))

	add := func(refs openapi3.Parameters) {
		for _, ref := range refs {
			if ref == nil || ref.Value == nil {
				continue
			}
			k := key{ref.Value.Name, ref.Value.In}
			if _, seen := merged[k]; !seen {
				order = append(order, k)
			}
			merged[k] = ref.Value
		}
	}
	add(pathLevel)
	add(opLevel) // operation level wins on conflict

	out := make([]*openapi3.Parameter, 0, len(order))
	for _, k := range order {
		out = append(out, merged[k])
	}
	return out
}

// synthesiseID builds a stable identifier for operations lacking an
// operationId, which is common in hand-written specs.
func synthesiseID(method, path string) string {
	cleaned := strings.NewReplacer("/", "_", "{", "", "}", "").Replace(path)
	cleaned = strings.Trim(cleaned, "_")
	if cleaned == "" {
		cleaned = "root"
	}
	return strings.ToLower(method) + "_" + cleaned
}

func isRemote(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Selector decides which operations a run should exercise.
type Selector interface {
	MethodAllowed(method string) bool
	PathAllowed(templatedPath string) bool
}

// Select filters operations through a Selector, returning the survivors and a
// per-reason count of what was excluded.
//
// The counts exist so the CLI can say "42 operations, 13 skipped (write
// methods)" at startup. Silently dropping most of a spec and then reporting
// healthy traffic is the failure mode most likely to waste someone's afternoon.
func (s *Spec) Select(sel Selector) (kept []*Operation, skippedByMethod, skippedByPath int) {
	for _, op := range s.Operations {
		switch {
		case !sel.MethodAllowed(op.Method):
			skippedByMethod++
		case !sel.PathAllowed(op.Path):
			skippedByPath++
		default:
			kept = append(kept, op)
		}
	}
	return kept, skippedByMethod, skippedByPath
}
