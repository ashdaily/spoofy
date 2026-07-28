package generator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ashdaily/spoofy/internal/openapi"
	"github.com/ashdaily/spoofy/internal/randomizer"
	"github.com/ashdaily/spoofy/internal/settings"
)

const specPath = "../openapi/testdata/petstore.yaml"

func loadSpec(t *testing.T) *openapi.Spec {
	t.Helper()
	spec, err := openapi.Load(context.Background(), specPath)
	if err != nil {
		t.Fatalf("loading spec: %v", err)
	}
	return spec
}

func op(t *testing.T, spec *openapi.Spec, method, path string) *openapi.Operation {
	t.Helper()
	for _, o := range spec.Operations {
		if o.Method == method && o.Path == path {
			return o
		}
	}
	t.Fatalf("no operation %s %s", method, path)
	return nil
}

func newGen(t *testing.T, baseURL string, auth settings.Auth) *Generator {
	t.Helper()
	g, err := New(baseURL, randomizer.New(42), auth)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func TestNewRejectsUnusableTargets(t *testing.T) {
	for _, target := range []string{"", "localhost:8080", "/just/a/path", "://nope"} {
		if _, err := New(target, randomizer.New(1), settings.Auth{}); err == nil {
			t.Errorf("New(%q) should have failed", target)
		}
	}
}

// A path template left unsubstituted 404s every single time, so this is the
// single most important property of the generator.
func TestPathParametersAreAlwaysSubstituted(t *testing.T) {
	spec := loadSpec(t)
	g := newGen(t, "http://localhost:8080", settings.Auth{})

	for _, o := range spec.Operations {
		for i := 0; i < 50; i++ {
			req, err := g.Build(context.Background(), o)
			if err != nil {
				t.Fatalf("%s: %v", o, err)
			}
			if strings.ContainsAny(req.URL.Path, "{}") {
				t.Fatalf("%s produced an unsubstituted path: %s", o, req.URL.Path)
			}
		}
	}
}

func TestQueryAndHeaderParameters(t *testing.T) {
	spec := loadSpec(t)
	g := newGen(t, "http://localhost:8080", settings.Auth{})
	listPets := op(t, spec, "GET", "/pets")

	var sawLimit, sawStatus, sawHeader bool
	for i := 0; i < 200; i++ {
		req, err := g.Build(context.Background(), listPets)
		if err != nil {
			t.Fatal(err)
		}
		q := req.URL.Query()
		if v := q.Get("limit"); v != "" {
			sawLimit = true
			// The schema bounds limit to [1, 100]; a float rendering like
			// "10.000000" would be rejected by most servers.
			if strings.Contains(v, ".") {
				t.Errorf("limit rendered as a float: %q", v)
			}
		}
		if v := q.Get("status"); v != "" {
			sawStatus = true
			switch v {
			case "available", "pending", "sold":
			default:
				t.Errorf("status %q is not in the spec enum", v)
			}
		}
		if req.Header.Get("X-Request-Id") != "" {
			sawHeader = true
		}
	}

	if !sawLimit || !sawStatus || !sawHeader {
		t.Errorf("optional parameters never appeared: limit=%v status=%v header=%v",
			sawLimit, sawStatus, sawHeader)
	}
}

// Optional parameters must vary. If they are always sent, the API's default
// code paths are never exercised.
func TestOptionalParametersAreSometimesOmitted(t *testing.T) {
	spec := loadSpec(t)
	g := newGen(t, "http://localhost:8080", settings.Auth{})
	listPets := op(t, spec, "GET", "/pets")

	var withLimit, withoutLimit int
	for i := 0; i < 200; i++ {
		req, _ := g.Build(context.Background(), listPets)
		if req.URL.Query().Get("limit") != "" {
			withLimit++
		} else {
			withoutLimit++
		}
	}
	if withLimit == 0 || withoutLimit == 0 {
		t.Errorf("limit present %d, absent %d; expected both", withLimit, withoutLimit)
	}
}

func TestRequestBodies(t *testing.T) {
	spec := loadSpec(t)
	g := newGen(t, "http://localhost:8080", settings.Auth{})

	t.Run("POST carries a valid JSON body", func(t *testing.T) {
		createPet := op(t, spec, "POST", "/pets")
		for i := 0; i < 100; i++ {
			req, err := g.Build(context.Background(), createPet)
			if err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}

			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("body is not valid JSON: %v (%s)", err, body)
			}
			// NewPet requires name and species.
			for _, key := range []string{"name", "species"} {
				if _, ok := decoded[key]; !ok {
					t.Fatalf("required field %q missing from body %s", key, body)
				}
			}
			// The spec pins NewPet.name to the example "Mango".
			if decoded["name"] != "Mango" {
				t.Errorf("name = %v, want the spec example Mango", decoded["name"])
			}
		}
	})

	t.Run("GET carries no body", func(t *testing.T) {
		req, err := g.Build(context.Background(), op(t, spec, "GET", "/pets"))
		if err != nil {
			t.Fatal(err)
		}
		if req.Body != nil {
			t.Error("GET request should have a nil body")
		}
		if req.Header.Get("Content-Type") != "" {
			t.Error("GET request should not set Content-Type")
		}
	})
}

func TestAuthIsApplied(t *testing.T) {
	spec := loadSpec(t)
	listPets := op(t, spec, "GET", "/pets")

	t.Run("bearer", func(t *testing.T) {
		g := newGen(t, "http://localhost:8080", settings.Auth{Bearer: "tok123"})
		req, _ := g.Build(context.Background(), listPets)
		if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("Authorization = %q", got)
		}
	})

	t.Run("basic", func(t *testing.T) {
		g := newGen(t, "http://localhost:8080", settings.Auth{
			Basic: settings.BasicAuth{User: "u", Pass: "p"},
		})
		req, _ := g.Build(context.Background(), listPets)
		user, pass, ok := req.BasicAuth()
		if !ok || user != "u" || pass != "p" {
			t.Errorf("BasicAuth = %q/%q ok=%v", user, pass, ok)
		}
	})

	// An explicit header must beat a spec-declared parameter of the same name,
	// otherwise a generated X-Request-Id could clobber a required API key.
	t.Run("explicit headers win", func(t *testing.T) {
		g := newGen(t, "http://localhost:8080", settings.Auth{
			Headers: map[string]string{"X-Request-Id": "fixed-value"},
		})
		for i := 0; i < 50; i++ {
			req, _ := g.Build(context.Background(), listPets)
			if got := req.Header.Get("X-Request-Id"); got != "fixed-value" {
				t.Fatalf("X-Request-Id = %q, want the configured value", got)
			}
		}
	})
}

func TestJoinPath(t *testing.T) {
	tests := []struct{ base, op, want string }{
		{"", "/pets", "/pets"},
		{"/", "/pets", "/pets"},
		{"/v1", "/pets", "/v1/pets"},
		{"/v1/", "/pets", "/v1/pets"},
		{"/v1", "pets", "/v1/pets"},
		{"/v1", "", "/v1"},
		{"", "", "/"},
		{"/api/v2", "/pets/{id}", "/api/v2/pets/{id}"},
	}
	for _, tc := range tests {
		if got := joinPath(tc.base, tc.op); got != tc.want {
			t.Errorf("joinPath(%q, %q) = %q, want %q", tc.base, tc.op, got, tc.want)
		}
	}
}

func TestToString(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"hello", "hello"},
		{true, "true"},
		{int64(42), "42"},
		{float64(10), "10"}, // whole floats must not become "10.000000"
		{float64(1.5), "1.5"},
		{[]any{"a", "b"}, "a,b"}, // simple-style array serialisation
		{map[string]any{"k": "v"}, `{"k":"v"}`},
	}
	for _, tc := range tests {
		if got := toString(tc.in); got != tc.want {
			t.Errorf("toString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCurlRedactsCredentials(t *testing.T) {
	spec := loadSpec(t)
	g := newGen(t, "http://localhost:8080", settings.Auth{Bearer: "super-secret-token"})
	req, _ := g.Build(context.Background(), op(t, spec, "GET", "/pets"))

	out := Curl(req, nil)
	if strings.Contains(out, "super-secret-token") {
		t.Error("Curl output leaked the bearer token")
	}
	if !strings.Contains(out, "<redacted>") {
		t.Error("Curl output should mark the redaction")
	}
	if !strings.Contains(out, "curl -i -X GET") {
		t.Errorf("Curl output does not look like a curl command:\n%s", out)
	}
}

func TestSameSeedProducesIdenticalRequests(t *testing.T) {
	spec := loadSpec(t)
	listPets := op(t, spec, "GET", "/pets")

	build := func() string {
		g := newGen(t, "http://localhost:8080", settings.Auth{})
		req, err := g.Build(context.Background(), listPets)
		if err != nil {
			t.Fatal(err)
		}
		return req.URL.String()
	}

	if first, second := build(), build(); first != second {
		t.Errorf("same seed diverged:\n  %s\n  %s", first, second)
	}
}

// Route every generated request through a mux implementing the spec's paths. A
// 404 here means the generator built a URL the API does not serve, which in
// production is a daemon reporting traffic while exercising nothing.
func TestGeneratedRequestsHitRealRoutes(t *testing.T) {
	var (
		mu   sync.Mutex
		hits = map[string]int{}
	)
	record := func(pattern string, status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[pattern]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status != http.StatusNoContent {
				w.Write([]byte(`{"ok":true}`))
			}
		}
	}

	mux := http.NewServeMux()
	routes := map[string]int{
		"GET /v1/pets":                200,
		"POST /v1/pets":               201,
		"GET /v1/pets/{petId}":        200,
		"DELETE /v1/pets/{petId}":     204,
		"PATCH /v1/pets/{petId}":      200,
		"GET /v1/pets/{petId}/photos": 200,
		"GET /v1/admin/users":         200,
		"GET /v1/health":              200,
	}
	for pattern, status := range routes {
		mux.HandleFunc(pattern, record(pattern, status))
	}
	// Anything unmatched is a generator bug, so make it loud.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no route for "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	spec := loadSpec(t)
	g := newGen(t, srv.URL+"/v1", settings.Auth{})
	client := srv.Client()

	for _, o := range spec.Operations {
		for i := 0; i < 25; i++ {
			req, err := g.Build(context.Background(), o)
			if err != nil {
				t.Fatalf("%s: build: %v", o, err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", o, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound {
				t.Fatalf("%s generated an unroutable request: %s %s -> %s",
					o, req.Method, req.URL.Path, strings.TrimSpace(string(body)))
			}
		}
	}

	// Every declared route should have been exercised at least once.
	mu.Lock()
	defer mu.Unlock()
	for pattern := range routes {
		if hits[pattern] == 0 {
			t.Errorf("route %s was never exercised", pattern)
		}
	}
}
