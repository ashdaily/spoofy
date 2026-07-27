package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func yamlUnmarshal(t *testing.T, in string, out any) error {
	t.Helper()
	return yaml.Unmarshal([]byte(in), out)
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: "20/s", want: 20},
		{in: "20", want: 20},
		{in: " 20 / s ", want: 20},
		{in: "1200/m", want: 20},
		{in: "1200/min", want: 20},
		{in: "72000/h", want: 20},
		{in: "0.5/s", want: 0.5},
		{in: "30/minute", want: 0.5},
		{in: "0", want: 0},
		{in: "", wantErr: true},
		{in: "fast", wantErr: true},
		{in: "20/fortnight", wantErr: true},
		{in: "-5/s", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRate(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRate(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRate(%q) unexpected error: %v", tc.in, err)
			}
			if got.PerSecond() != tc.want {
				t.Errorf("ParseRate(%q) = %v/s, want %v/s", tc.in, got.PerSecond(), tc.want)
			}
		})
	}
}

// Rates written in per-minute terms should not echo back as tiny per-second
// decimals; that mismatch between what you wrote and what the tool says back is
// a small thing that erodes trust in every other number it reports.
func TestRateStringUsesNaturalUnit(t *testing.T) {
	tests := []struct {
		rate Rate
		want string
	}{
		{rate: 20, want: "20/s"},
		{rate: 1, want: "1/s"},
		{rate: 0.5, want: "30/m"},
		{rate: 0.01, want: "36/h"},
		{rate: 0, want: "0/s"},
	}
	for _, tc := range tests {
		if got := tc.rate.String(); got != tc.want {
			t.Errorf("Rate(%v).String() = %q, want %q", float64(tc.rate), got, tc.want)
		}
	}
}

func TestLooksLikeProduction(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{host: "api.prod.acme.com", want: true},
		{host: "api-prod.acme.com", want: true},
		{host: "production.acme.com", want: true},
		{host: "prd.acme.com", want: true},
		{host: "api.prod.acme.com:8443", want: true},

		{host: "staging.acme.com", want: false},
		{host: "localhost", want: false},
		{host: "localhost:8080", want: false},
		{host: "api.dev.acme.com", want: false},
		{host: "qa-api.acme.com", want: false},

		// A bare hostname carries no marker either way. Refusing here would
		// fire during correct usage, so it must not.
		{host: "api.acme.com", want: false},
		{host: "10.0.0.5", want: false},

		// Substring false positives the token split is meant to prevent.
		{host: "prodigy.com", want: false},
		{host: "reproduction.example.com", want: false},

		// An explicit non-prod marker overrides a prod token.
		{host: "staging-prod-mirror.acme.com", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := LooksLikeProduction(tc.host); got != tc.want {
				t.Errorf("LooksLikeProduction(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{pattern: "/orders", path: "/orders", want: true},
		{pattern: "/orders", path: "/orders/{id}", want: false},

		// The behaviour path.Match would get wrong: "*" must span "/".
		{pattern: "/admin/*", path: "/admin/users/{id}", want: true},
		{pattern: "/admin/*", path: "/admin", want: false},
		{pattern: "/admin*", path: "/admin", want: true},

		{pattern: "*", path: "/anything/at/all", want: true},
		{pattern: "*/{id}", path: "/orders/{id}", want: true},
		{pattern: "/orders/*/items", path: "/orders/{id}/items", want: true},
		{pattern: "/orders/*/items", path: "/orders/{id}/items/{sku}", want: false},
		{pattern: "", path: "/orders", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.pattern+" vs "+tc.path, func(t *testing.T) {
			if got := matchGlob(tc.pattern, tc.path); got != tc.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestEndpointRules(t *testing.T) {
	cfg := Default()
	cfg.Endpoints = []EndpointRule{
		{Match: "/admin/*", Skip: true},
		{Match: "/orders", Weight: 5},
		{Match: "/health", Weight: 0}, // weight 0 is an exclusion
	}

	if cfg.PathAllowed("/admin/users") {
		t.Error("expected /admin/users to be skipped")
	}
	if !cfg.PathAllowed("/orders") {
		t.Error("expected /orders to be allowed")
	}
	if cfg.PathAllowed("/health") {
		t.Error("expected weight-0 /health to be excluded")
	}
	if got := cfg.WeightFor("/orders"); got != 5 {
		t.Errorf("WeightFor(/orders) = %v, want 5", got)
	}
	if got := cfg.WeightFor("/unlisted"); got != 1 {
		t.Errorf("WeightFor(/unlisted) = %v, want 1 (default)", got)
	}
}

// First match wins, so an earlier broad rule shadows a later specific one.
// This is the documented behaviour and worth pinning: if it ever flips to
// last-match-wins, every existing config silently changes meaning.
func TestEndpointRuleOrderIsFirstMatchWins(t *testing.T) {
	cfg := Default()
	cfg.Endpoints = []EndpointRule{
		{Match: "/orders*", Weight: 2},
		{Match: "/orders/{id}", Weight: 99},
	}
	if got := cfg.WeightFor("/orders/{id}"); got != 2 {
		t.Errorf("WeightFor = %v, want 2 (first matching rule)", got)
	}
}

func TestMethodAllowed(t *testing.T) {
	cfg := Default()

	for _, m := range []string{"GET", "HEAD", "OPTIONS", "get"} {
		if !cfg.MethodAllowed(m) {
			t.Errorf("expected %s to be allowed by default", m)
		}
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if cfg.MethodAllowed(m) {
			t.Errorf("expected %s to be blocked without allow_writes", m)
		}
	}

	cfg.Safety.AllowWrites = true
	if !cfg.MethodAllowed("DELETE") {
		t.Error("expected DELETE to be allowed once allow_writes is set")
	}
}

func TestValidate(t *testing.T) {
	valid := func() Config {
		c := Default()
		c.Target = "http://localhost:8080"
		c.Spec = "openapi.yaml"
		return c
	}

	t.Run("accepts a minimal valid config", func(t *testing.T) {
		c := valid()
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("refuses a production target", func(t *testing.T) {
		c := valid()
		c.Target = "https://api.prod.acme.com"
		err := c.Validate()
		if err == nil {
			t.Fatal("expected refusal for a production-looking target")
		}
		if !strings.Contains(err.Error(), "allow_prod") {
			t.Errorf("error should name the override to use, got: %v", err)
		}
	})

	t.Run("allows production when explicitly permitted", func(t *testing.T) {
		c := valid()
		c.Target = "https://api.prod.acme.com"
		c.Safety.AllowProd = true
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("enforces the rate ceiling", func(t *testing.T) {
		c := valid()
		c.Traffic.Rate = 5000
		err := c.Validate()
		if err == nil {
			t.Fatal("expected the rate ceiling to reject 5000/s")
		}
		if !strings.Contains(err.Error(), "max_rate") {
			t.Errorf("error should name max_rate, got: %v", err)
		}
	})

	t.Run("rejects an unknown shape and names the valid ones", func(t *testing.T) {
		c := valid()
		c.Traffic.Shape = "sawtooth"
		err := c.Validate()
		if err == nil {
			t.Fatal("expected an unknown shape to be rejected")
		}
		if !strings.Contains(err.Error(), ShapeDiurnal) {
			t.Errorf("error should list valid shapes, got: %v", err)
		}
	})

	t.Run("ramp requires its parameters", func(t *testing.T) {
		c := valid()
		c.Traffic.Shape = ShapeRamp
		err := c.Validate()
		if err == nil {
			t.Fatal("expected ramp without over/to to be rejected")
		}
		for _, want := range []string{"over", "to"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name the missing key %q, got: %v", want, err)
			}
		}
	})

	t.Run("spike must be shorter than its interval", func(t *testing.T) {
		c := valid()
		c.Traffic.Shape = ShapeSpike
		c.Traffic.SpikeEvery = Duration(time.Minute)
		c.Traffic.SpikeFor = Duration(2 * time.Minute)
		c.Traffic.SpikeRate = 50
		err := c.Validate()
		if err == nil {
			t.Fatal("expected a spike longer than its interval to be rejected")
		}
		if !strings.Contains(err.Error(), "never ends") {
			t.Errorf("error should explain why, got: %v", err)
		}
	})

	// Validate should surface every problem in one pass. Making someone restart
	// a daemon four times to find four typos is the failure mode this prevents.
	t.Run("reports multiple problems at once", func(t *testing.T) {
		c := Default()
		c.Traffic.Concurrency = -1
		c.Traffic.Shape = "nonsense"
		err := c.Validate()
		if err == nil {
			t.Fatal("expected errors")
		}
		msg := err.Error()
		for _, want := range []string{"target", "spec", "concurrency", "shape"} {
			if !strings.Contains(msg, want) {
				t.Errorf("expected all problems reported together, %q missing from: %v", want, msg)
			}
		}
	})
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("SPOOFY_TEST_TOKEN", "s3cret")

	got := string(ExpandEnv([]byte("bearer: ${SPOOFY_TEST_TOKEN}")))
	if got != "bearer: s3cret" {
		t.Errorf("got %q, want %q", got, "bearer: s3cret")
	}

	// Unset variables become empty rather than erroring, so a config can carry
	// optional credentials without branching.
	if got := string(ExpandEnv([]byte("bearer: ${SPOOFY_UNSET_VAR}"))); got != "bearer: " {
		t.Errorf("unset var: got %q", got)
	}

	// Bare $NAME is left alone. Passwords contain "$" often enough that
	// expanding it would corrupt credentials in hard-to-debug ways.
	const literal = "pass: hunter$2"
	if got := string(ExpandEnv([]byte(literal))); got != literal {
		t.Errorf("bare $ should not expand: got %q, want %q", got, literal)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPOOFY_TEST_TOKEN", "from-env")

	write := func(t *testing.T, name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("parses a full config", func(t *testing.T) {
		p := write(t, "spoofy.yaml", `
target: http://localhost:8080
spec: ./openapi.yaml

traffic:
  rate: 1200/m
  shape: diurnal
  amplitude: 0.5
  period: 24h
  concurrency: 4
  timeout: 5s

endpoints:
  - match: /admin/*
    skip: true
  - match: /orders
    weight: 5

auth:
  bearer: ${SPOOFY_TEST_TOKEN}
  headers:
    X-Tenant: acme

safety:
  allow_writes: true
  max_rate: 500/s

metrics:
  addr: ":9109"
`)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}

		if got := cfg.Traffic.Rate.PerSecond(); got != 20 {
			t.Errorf("rate = %v/s, want 20/s (from 1200/m)", got)
		}
		if cfg.Traffic.Shape != ShapeDiurnal {
			t.Errorf("shape = %q", cfg.Traffic.Shape)
		}
		if cfg.Traffic.Period.D() != 24*time.Hour {
			t.Errorf("period = %v", cfg.Traffic.Period.D())
		}
		if cfg.Traffic.Timeout.D() != 5*time.Second {
			t.Errorf("timeout = %v", cfg.Traffic.Timeout.D())
		}
		if cfg.Auth.Bearer != "from-env" {
			t.Errorf("bearer = %q, want the expanded env value", cfg.Auth.Bearer)
		}
		if cfg.Auth.Headers["X-Tenant"] != "acme" {
			t.Errorf("headers = %v", cfg.Auth.Headers)
		}
		if cfg.Safety.MaxRate.PerSecond() != 500 {
			t.Errorf("max_rate = %v", cfg.Safety.MaxRate)
		}
		if len(cfg.Endpoints) != 2 {
			t.Fatalf("endpoints = %d, want 2", len(cfg.Endpoints))
		}
	})

	// A silently-ignored typo produces a daemon that runs for a week doing the
	// wrong thing. Unknown keys must be loud.
	t.Run("rejects unknown keys", func(t *testing.T) {
		p := write(t, "typo.yaml", `
target: http://localhost:8080
spec: ./openapi.yaml
traffic:
  raet: 20/s
`)
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected a typo'd key to be rejected")
		}
		if !strings.Contains(err.Error(), "raet") {
			t.Errorf("error should name the offending key, got: %v", err)
		}
	})

	t.Run("applies defaults to a near-empty config", func(t *testing.T) {
		p := write(t, "minimal.yaml", "target: http://localhost:8080\nspec: ./openapi.yaml\n")
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Traffic.Rate != DefaultRate {
			t.Errorf("rate = %v, want default %v", cfg.Traffic.Rate, DefaultRate)
		}
		if cfg.Traffic.Shape != ShapeConstant {
			t.Errorf("shape = %q, want %q", cfg.Traffic.Shape, ShapeConstant)
		}
		if cfg.Metrics.Addr != DefaultMetricsAddr {
			t.Errorf("metrics.addr = %q", cfg.Metrics.Addr)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("a minimal config should be valid: %v", err)
		}
	})

	t.Run("reports a bad rate with the offending text", func(t *testing.T) {
		p := write(t, "badrate.yaml", "target: http://localhost:8080\ntraffic:\n  rate: quickly\n")
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected an unparseable rate to fail")
		}
		if !strings.Contains(err.Error(), "quickly") {
			t.Errorf("error should quote the bad value, got: %v", err)
		}
	})
}

func TestDiscovery(t *testing.T) {
	dir := t.TempDir()
	if got := DiscoverConfig(dir); got != "" {
		t.Errorf("expected no config in an empty dir, got %q", got)
	}
	if got := DiscoverSpec(dir); got != "" {
		t.Errorf("expected no spec in an empty dir, got %q", got)
	}

	specPath := filepath.Join(dir, "openapi.yaml")
	if err := os.WriteFile(specPath, []byte("openapi: 3.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverSpec(dir); got != specPath {
		t.Errorf("DiscoverSpec = %q, want %q", got, specPath)
	}

	cfgPath := filepath.Join(dir, "spoofy.yaml")
	if err := os.WriteFile(cfgPath, []byte("target: http://x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverConfig(dir); got != cfgPath {
		t.Errorf("DiscoverConfig = %q, want %q", got, cfgPath)
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var cfg struct {
		D Duration `yaml:"d"`
	}
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{in: "d: 30s", want: 30 * time.Second},
		{in: "d: 1h30m", want: 90 * time.Minute},
		{in: "d: 45", want: 45 * time.Second},
	} {
		if err := yamlUnmarshal(t, tc.in, &cfg); err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if cfg.D.D() != tc.want {
			t.Errorf("%q = %v, want %v", tc.in, cfg.D.D(), tc.want)
		}
	}

	if err := yamlUnmarshal(t, "d: soon", &cfg); err == nil {
		t.Error("expected an invalid duration to be rejected")
	}
}
