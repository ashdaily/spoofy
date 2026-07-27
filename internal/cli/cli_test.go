package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/ashdaily/spoofy/internal/settings"
)

// resolveWith parses args through the real run command and returns the merged
// config, so flag wiring is exercised rather than bypassed.
func resolveWith(t *testing.T, args ...string) (*settings.Config, error) {
	t.Helper()

	var captured *settings.Config
	var capturedErr error

	cmd := newRunCmd()
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		// f is captured by newRunCmd's closure; re-resolve through the same path.
		captured, capturedErr = resolve(c)
		return nil
	}
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	if err := cmd.Execute(); err != nil {
		return nil, err
	}
	return captured, capturedErr
}

func TestConfigDiscoveryAndDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	writeFile(t, filepath.Join(dir, "openapi.yaml"), "openapi: 3.0.0\n")

	cfg, err := resolveWith(t, "--url", "http://localhost:8080")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The spec should be discovered without being named.
	if !strings.HasSuffix(cfg.Spec, "openapi.yaml") {
		t.Errorf("Spec = %q, want the discovered openapi.yaml", cfg.Spec)
	}
	if cfg.Traffic.Rate != settings.DefaultRate {
		t.Errorf("Rate = %v, want default", cfg.Traffic.Rate)
	}
	if cfg.Traffic.Shape != settings.ShapeConstant {
		t.Errorf("Shape = %q", cfg.Traffic.Shape)
	}
	if cfg.Safety.AllowWrites {
		t.Error("writes must be off by default")
	}
}

// A config file is found without being named, and flags then override it.
// Someone typing a flag is being more specific than a file checked in months
// ago, so the flag has to win.
func TestFlagsOverrideConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	writeFile(t, filepath.Join(dir, "spoofy.yaml"), `
target: http://from-file:9999
spec: ./from-file.yaml
traffic:
  rate: 5/s
  shape: constant
  concurrency: 2
`)

	cfg, err := resolveWith(t,
		"--url", "http://from-flag:8080",
		"--rate", "1200/m",
		"--shape", "diurnal",
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if cfg.Target != "http://from-flag:8080" {
		t.Errorf("Target = %q, want the flag value", cfg.Target)
	}
	if got := cfg.Traffic.Rate.PerSecond(); got != 20 {
		t.Errorf("Rate = %v/s, want 20/s from --rate 1200/m", got)
	}
	if cfg.Traffic.Shape != settings.ShapeDiurnal {
		t.Errorf("Shape = %q, want the flag value", cfg.Traffic.Shape)
	}
	// Untouched by flags, so the file still governs.
	if cfg.Traffic.Concurrency != 2 {
		t.Errorf("Concurrency = %d, want 2 from the config file", cfg.Traffic.Concurrency)
	}
	if cfg.Spec != "./from-file.yaml" {
		t.Errorf("Spec = %q, want the file value", cfg.Spec)
	}
}

func TestAuthFlags(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, filepath.Join(dir, "openapi.yaml"), "openapi: 3.0.0\n")

	cfg, err := resolveWith(t,
		"--url", "http://localhost:8080",
		"--auth-bearer", "tok",
		"--header", "X-Tenant: acme",
		"--header", "X-Trace: on",
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if cfg.Auth.Bearer != "tok" {
		t.Errorf("Bearer = %q", cfg.Auth.Bearer)
	}
	if cfg.Auth.Headers["X-Tenant"] != "acme" || cfg.Auth.Headers["X-Trace"] != "on" {
		t.Errorf("Headers = %v", cfg.Auth.Headers)
	}

	t.Run("basic auth must be user:pass", func(t *testing.T) {
		_, err := resolveWith(t, "--url", "http://localhost:8080", "--auth-basic", "nocolon")
		if err == nil {
			t.Fatal("expected an error for malformed --auth-basic")
		}
		if !strings.Contains(err.Error(), "user:pass") {
			t.Errorf("error should show the expected form, got: %v", err)
		}
	})
}

// --only means "only these", so anything unnamed has to be excluded.
func TestOnlyAndSkipFilters(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, filepath.Join(dir, "openapi.yaml"), "openapi: 3.0.0\n")

	t.Run("skip excludes", func(t *testing.T) {
		cfg, err := resolveWith(t, "--url", "http://localhost:8080", "--skip", "/admin/*")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PathAllowed("/admin/users") {
			t.Error("/admin/users should be skipped")
		}
		if !cfg.PathAllowed("/pets") {
			t.Error("/pets should still be allowed")
		}
	})

	t.Run("only excludes everything unnamed", func(t *testing.T) {
		cfg, err := resolveWith(t, "--url", "http://localhost:8080", "--only", "/pets")
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.PathAllowed("/pets") {
			t.Error("/pets was named in --only and should be allowed")
		}
		if cfg.PathAllowed("/orders") {
			t.Error("/orders was not named in --only and should be excluded")
		}
	})
}

func TestRateFlagRejectsNonsense(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, filepath.Join(dir, "openapi.yaml"), "openapi: 3.0.0\n")

	_, err := resolveWith(t, "--url", "http://localhost:8080", "--rate", "very fast")
	if err == nil {
		t.Fatal("expected an unparseable rate to fail")
	}
	if !strings.Contains(err.Error(), "very fast") {
		t.Errorf("error should quote the bad value, got: %v", err)
	}
}

// The production guard has to survive the whole flag path, not just unit tests
// on settings.
func TestProductionTargetIsRefusedThroughTheCLI(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, filepath.Join(dir, "openapi.yaml"), "openapi: 3.0.0\n")

	cfg, err := resolveWith(t, "--url", "https://api.prod.acme.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a production-looking target should be refused")
	}

	cfg, err = resolveWith(t, "--url", "https://api.prod.acme.com", "--allow-prod")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("--allow-prod should permit it: %v", err)
	}
}

func TestInitWritesAUsableConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	out := &bytes.Buffer{}
	cmd := newInitCmd()
	cmd.SetOut(out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	body, err := os.ReadFile("spoofy.yaml")
	if err != nil {
		t.Fatalf("init did not write spoofy.yaml: %v", err)
	}

	// The generated file must actually load and validate — a starter config
	// that errors on first run is worse than no starter config.
	cfg, err := settings.Load("spoofy.yaml")
	if err != nil {
		t.Fatalf("generated config does not parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("generated config does not validate: %v", err)
	}

	// Every documented shape should be mentioned, since the file is the
	// primary place people learn the format.
	for _, shape := range settings.ValidShapes {
		if !strings.Contains(string(body), shape) {
			t.Errorf("generated config never mentions the %q shape", shape)
		}
	}

	t.Run("refuses to clobber without --force", func(t *testing.T) {
		cmd := newInitCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err == nil {
			t.Fatal("expected init to refuse overwriting an existing file")
		}
	})

	t.Run("--force overwrites", func(t *testing.T) {
		cmd := newInitCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--force"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("--force should overwrite: %v", err)
		}
	})
}

func TestVersionCommand(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newVersionCmd()
	cmd.SetOut(out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "spoofy") {
		t.Errorf("version output = %q", out.String())
	}
}

func TestDurationFlagParses(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, filepath.Join(dir, "openapi.yaml"), "openapi: 3.0.0\n")

	cmd := newRunCmd()
	cmd.RunE = func(c *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"--url", "http://localhost:8080", "--duration", "90s"})
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	got, err := cmd.Flags().GetDuration("duration")
	if err != nil {
		t.Fatal(err)
	}
	if got != 90*time.Second {
		t.Errorf("duration = %v, want 90s", got)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
