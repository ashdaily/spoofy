package openapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const testSpec = "testdata/petstore.yaml"

func load(t *testing.T) *Spec {
	t.Helper()
	spec, err := Load(context.Background(), testSpec)
	if err != nil {
		t.Fatalf("Load(%s): %v", testSpec, err)
	}
	return spec
}

func find(t *testing.T, spec *Spec, method, path string) *Operation {
	t.Helper()
	for _, op := range spec.Operations {
		if op.Method == method && op.Path == path {
			return op
		}
	}
	t.Fatalf("operation %s %s not found in spec", method, path)
	return nil
}

func TestLoadMetadata(t *testing.T) {
	spec := load(t)

	if spec.Title != "Spoofy Test Store" {
		t.Errorf("Title = %q", spec.Title)
	}
	if spec.Version != "1.4.2" {
		t.Errorf("Version = %q", spec.Version)
	}
	if len(spec.Servers) != 2 {
		t.Errorf("Servers = %v, want 2 entries", spec.Servers)
	}
}

func TestLoadEnumeratesEveryOperation(t *testing.T) {
	spec := load(t)

	want := []string{
		"GET /admin/users",
		"GET /health",
		"GET /pets",
		"POST /pets",
		"DELETE /pets/{petId}",
		"GET /pets/{petId}",
		"PATCH /pets/{petId}",
		"GET /pets/{petId}/photos",
	}
	if len(spec.Operations) != len(want) {
		var got []string
		for _, op := range spec.Operations {
			got = append(got, op.String())
		}
		t.Fatalf("got %d operations %v, want %d", len(spec.Operations), got, len(want))
	}

	seen := make(map[string]bool)
	for _, op := range spec.Operations {
		seen[op.String()] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("missing operation %s", w)
		}
	}
}

// Path-item-level parameters apply to every operation beneath them. Dropping
// them is a quiet failure: the operation still builds, it just omits a required
// path parameter and every request 404s forever.
func TestPathLevelParametersAreInherited(t *testing.T) {
	spec := load(t)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/pets/{petId}"},
		{"DELETE", "/pets/{petId}"},
		{"PATCH", "/pets/{petId}"},
		{"GET", "/pets/{petId}/photos"},
	} {
		op := find(t, spec, tc.method, tc.path)
		var found bool
		for _, p := range op.Params {
			if p.Name == "petId" && p.In == "path" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: inherited petId path parameter missing", op)
		}
	}
}

// An operation-level parameter replaces an inherited one with the same
// name+location rather than duplicating it.
func TestOperationParametersOverrideInherited(t *testing.T) {
	spec := load(t)
	op := find(t, spec, "PATCH", "/pets/{petId}")

	var petIDParams int
	for _, p := range op.Params {
		if p.Name == "petId" && p.In == "path" {
			petIDParams++
			if p.Example == nil {
				t.Error("expected the operation-level petId (which carries an example) to win")
			}
		}
	}
	if petIDParams != 1 {
		t.Errorf("petId appears %d times, want exactly 1", petIDParams)
	}

	var hasDryRun bool
	for _, p := range op.Params {
		if p.Name == "dry_run" && p.In == "query" {
			hasDryRun = true
		}
	}
	if !hasDryRun {
		t.Error("operation-level dry_run query parameter missing")
	}
}

func TestQueryAndHeaderParameters(t *testing.T) {
	spec := load(t)
	op := find(t, spec, "GET", "/pets")

	byName := make(map[string]string) // name -> in
	for _, p := range op.Params {
		byName[p.Name] = p.In
	}
	for name, wantIn := range map[string]string{
		"limit":        "query",
		"status":       "query",
		"X-Request-Id": "header",
	} {
		if got := byName[name]; got != wantIn {
			t.Errorf("parameter %q in = %q, want %q", name, got, wantIn)
		}
	}
}

func TestSynthesisedOperationID(t *testing.T) {
	spec := load(t)

	op := find(t, spec, "PATCH", "/pets/{petId}")
	if op.ID != "patch_pets_petId" {
		t.Errorf("synthesised ID = %q, want %q", op.ID, "patch_pets_petId")
	}

	// A spec-provided operationId must be preserved untouched.
	if got := find(t, spec, "GET", "/pets").ID; got != "listPets" {
		t.Errorf("ID = %q, want listPets", got)
	}
}

func TestDeprecatedAndRequestBody(t *testing.T) {
	spec := load(t)

	if !find(t, spec, "PATCH", "/pets/{petId}").Deprecated {
		t.Error("expected PATCH /pets/{petId} to be marked deprecated")
	}
	if find(t, spec, "GET", "/pets").Deprecated {
		t.Error("GET /pets is not deprecated")
	}

	post := find(t, spec, "POST", "/pets")
	if post.RequestBody == nil {
		t.Fatal("POST /pets should carry a request body")
	}
	if post.RequestBody.Content.Get("application/json") == nil {
		t.Error("expected an application/json body schema")
	}

	if find(t, spec, "GET", "/pets").RequestBody != nil {
		t.Error("GET /pets should have no request body")
	}
}

// $ref indirection must be resolved by load time; nothing downstream should
// have to think about references.
func TestRefsAreResolved(t *testing.T) {
	spec := load(t)
	body := find(t, spec, "POST", "/pets").RequestBody

	media := body.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		t.Fatal("request body schema did not resolve")
	}
	props := media.Schema.Value.Properties
	if _, ok := props["name"]; !ok {
		t.Errorf("NewPet.name missing; properties = %v", propNames(props))
	}
	// Nested $ref (NewPet.owner -> Owner) must resolve too.
	owner, ok := props["owner"]
	if !ok || owner.Value == nil {
		t.Fatal("NewPet.owner did not resolve")
	}
	if _, ok := owner.Value.Properties["email"]; !ok {
		t.Error("Owner.email missing after nested $ref resolution")
	}
}

func TestOperationsAreOrderedDeterministically(t *testing.T) {
	first := load(t)
	second := load(t)

	for i := range first.Operations {
		if first.Operations[i].String() != second.Operations[i].String() {
			t.Fatalf("ordering differs at %d: %s vs %s",
				i, first.Operations[i], second.Operations[i])
		}
	}
}

type selector struct {
	methods map[string]bool
	skip    []string
}

func (s selector) MethodAllowed(m string) bool { return s.methods[m] }
func (s selector) PathAllowed(p string) bool {
	for _, prefix := range s.skip {
		if strings.HasPrefix(p, prefix) {
			return false
		}
	}
	return true
}

func TestSelectReportsWhatItDropped(t *testing.T) {
	spec := load(t)

	kept, byMethod, byPath := spec.Select(selector{
		methods: map[string]bool{"GET": true},
		skip:    []string{"/admin"},
	})

	// 8 total: 5 GETs, of which /admin/users is path-skipped; 3 non-GET.
	if len(kept) != 4 {
		var names []string
		for _, op := range kept {
			names = append(names, op.String())
		}
		t.Errorf("kept %d operations %v, want 4", len(kept), names)
	}
	if byMethod != 3 {
		t.Errorf("skippedByMethod = %d, want 3 (POST, DELETE, PATCH)", byMethod)
	}
	if byPath != 1 {
		t.Errorf("skippedByPath = %d, want 1 (/admin/users)", byPath)
	}
}

func TestLoadErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("missing file", func(t *testing.T) {
		_, err := Load(ctx, filepath.Join(t.TempDir(), "nope.yaml"))
		if err == nil {
			t.Fatal("expected an error for a missing spec")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should say the file is missing, got: %v", err)
		}
	})

	t.Run("spec with no paths", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "empty.yaml")
		os.WriteFile(p, []byte("openapi: 3.0.0\ninfo:\n  title: x\n  version: \"1\"\npaths: {}\n"), 0o644)
		_, err := Load(ctx, p)
		if err == nil {
			t.Fatal("expected an error for a spec with no operations")
		}
	})

	t.Run("malformed yaml", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.yaml")
		os.WriteFile(p, []byte("openapi: 3.0.0\npaths: [this is not a map\n"), 0o644)
		if _, err := Load(ctx, p); err == nil {
			t.Fatal("expected a parse error")
		}
	})
}

// Specs are commonly fetched straight from the running service, so loading over
// HTTP is a first-class path rather than a convenience.
func TestLoadFromHTTP(t *testing.T) {
	body, err := os.ReadFile(testSpec)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openapi.yaml" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(body)
	}))
	defer srv.Close()

	spec, err := Load(context.Background(), srv.URL+"/openapi.yaml")
	if err != nil {
		t.Fatalf("Load over HTTP: %v", err)
	}
	if len(spec.Operations) != 8 {
		t.Errorf("got %d operations over HTTP, want 8", len(spec.Operations))
	}

	if _, err := Load(context.Background(), srv.URL+"/missing.yaml"); err == nil {
		t.Error("expected an error for a 404 spec URL")
	}
}

func propNames(m openapi3.Schemas) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
