// Package generator turns a spec operation into a concrete HTTP request.
//
// The rules it follows are OpenAPI's, but the priority is Spoofy's: a request
// the target will accept beats one that merely type-checks.
package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/ashdaily/spoofy/internal/openapi"
	"github.com/ashdaily/spoofy/internal/randomizer"
	"github.com/ashdaily/spoofy/internal/settings"
)

// preferredContentTypes are tried in order when an operation offers several.
// JSON first, since it is what most APIs speak and what Spoofy generates most
// faithfully.
var preferredContentTypes = []string{
	"application/json",
	"application/x-www-form-urlencoded",
	"text/plain",
}

// Generator builds requests for a single target. Like randomizer.Randomizer it
// is not safe for concurrent use; give each worker its own.
type Generator struct {
	base *url.URL
	rnd  *randomizer.Randomizer
	auth settings.Auth
}

// New returns a Generator sending to baseURL.
func New(baseURL string, rnd *randomizer.Randomizer, auth settings.Auth) (*Generator, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("target URL %q is not valid: %w", baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("target URL %q needs a scheme and host, e.g. http://localhost:8080", baseURL)
	}
	return &Generator{base: u, rnd: rnd, auth: auth}, nil
}

// Build produces a request for the operation.
func (g *Generator) Build(ctx context.Context, op *openapi.Operation) (*http.Request, error) {
	path, query, headers, cookies := g.params(op)

	target := *g.base
	target.Path = joinPath(g.base.Path, path)
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}

	body, contentType, err := g.body(op)
	if err != nil {
		return nil, fmt.Errorf("%s: building request body: %w", op, err)
	}

	// A nil reader, not an empty one. An empty bytes.Reader still sets
	// Content-Length: 0 on a GET, which some strict servers reject.
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, op.Method, target.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "spoofy")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}

	// Auth last, so it wins over any spec-declared header of the same name.
	g.auth.Apply(req)

	return req, nil
}

// params generates values for every declared parameter, grouped by location.
func (g *Generator) params(op *openapi.Operation) (path string, query url.Values, headers map[string]string, cookies []*http.Cookie) {
	path = op.Path
	query = url.Values{}
	headers = map[string]string{}

	for _, p := range op.Params {
		if p == nil {
			continue
		}
		// Omit optional parameters sometimes, or the API's filters are never
		// exercised. Path parameters are exempt: omitting one leaves a literal
		// "{id}" in the URL, which 404s every time.
		if !p.Required && p.In != "path" && !g.rnd.Bool() {
			continue
		}

		value := g.paramValue(p)
		if value == "" && p.In != "path" && !p.Required {
			continue
		}

		switch p.In {
		case "path":
			if value == "" {
				// A path parameter with no usable schema would leave the
				// template intact and guarantee a 404.
				value = g.rnd.String(&openapi3.Schema{Type: &openapi3.Types{"string"}})
			}
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(value))
		case "query":
			query.Set(p.Name, value)
		case "header":
			headers[p.Name] = value
		case "cookie":
			cookies = append(cookies, &http.Cookie{Name: p.Name, Value: value})
		}
	}

	return path, query, headers, cookies
}

// paramValue resolves a single parameter, preferring what the spec says over
// anything generated.
func (g *Generator) paramValue(p *openapi3.Parameter) string {
	if p.Example != nil {
		return toString(p.Example)
	}
	if len(p.Examples) > 0 {
		for _, name := range sortedExampleKeys(p.Examples) {
			if ex := p.Examples[name]; ex != nil && ex.Value != nil {
				return toString(ex.Value.Value)
			}
		}
	}
	if p.Schema != nil && p.Schema.Value != nil {
		return toString(g.rnd.Value(p.Schema.Value))
	}
	return ""
}

// body builds a request body, returning nil when the operation takes none.
func (g *Generator) body(op *openapi.Operation) ([]byte, string, error) {
	if op.RequestBody == nil || len(op.RequestBody.Content) == 0 {
		return nil, "", nil
	}

	contentType, media := pickContent(op.RequestBody.Content)
	if media == nil {
		return nil, "", nil
	}

	// A spec-provided body example is a real request someone wrote down.
	var value any
	switch {
	case media.Example != nil:
		value = media.Example
	case len(media.Examples) > 0:
		for _, name := range sortedExampleKeys(media.Examples) {
			if ex := media.Examples[name]; ex != nil && ex.Value != nil {
				value = ex.Value.Value
				break
			}
		}
	case media.Schema != nil && media.Schema.Value != nil:
		value = g.rnd.Value(media.Schema.Value)
	default:
		return nil, "", nil
	}

	switch {
	case strings.Contains(contentType, "json"):
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, "", err
		}
		return encoded, contentType, nil

	case strings.Contains(contentType, "x-www-form-urlencoded"):
		form := url.Values{}
		if m, ok := value.(map[string]any); ok {
			for _, k := range sortedKeys(m) {
				form.Set(k, toString(m[k]))
			}
		}
		return []byte(form.Encode()), contentType, nil

	default:
		return []byte(toString(value)), contentType, nil
	}
}

// pickContent chooses a media type, preferring ones Spoofy generates well.
func pickContent(content openapi3.Content) (string, *openapi3.MediaType) {
	for _, want := range preferredContentTypes {
		if media := content.Get(want); media != nil {
			return want, media
		}
	}
	// Sorted, so the same spec always picks the same media type instead of
	// whatever map iteration surfaced first.
	for _, name := range sortedContentKeys(content) {
		return name, content[name]
	}
	return "", nil
}

// Curl renders the request as a copy-pasteable curl command, which is worth
// more in dry-run output than a description of what would have been sent.
func Curl(req *http.Request, body []byte) string {
	var b strings.Builder
	b.WriteString("curl -i -X " + req.Method)

	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := req.Header.Get(name)
		if name == "Authorization" {
			value = "<redacted>"
		}
		b.WriteString(fmt.Sprintf(" \\\n  -H %q", name+": "+value))
	}

	if len(body) > 0 {
		b.WriteString(fmt.Sprintf(" \\\n  -d %q", string(body)))
	}
	b.WriteString(fmt.Sprintf(" \\\n  %q", req.URL.String()))
	return b.String()
}

// joinPath splices a base path onto an operation path without doubling or
// dropping the separator. http://host/v1 plus /pets must give /v1/pets, which
// neither concatenation nor path.Join gets right in every case.
func joinPath(basePath, opPath string) string {
	basePath = strings.TrimSuffix(basePath, "/")
	if opPath == "" {
		if basePath == "" {
			return "/"
		}
		return basePath
	}
	if !strings.HasPrefix(opPath, "/") {
		opPath = "/" + opPath
	}
	return basePath + opPath
}

// toString renders a generated value for use in a URL or header.
func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		// Whole floats render as integers: "limit=10", not "limit=10.000000".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, toString(item))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		encoded, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(encoded)
	default:
		return fmt.Sprint(t)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedExampleKeys(m openapi3.Examples) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedContentKeys(m openapi3.Content) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
