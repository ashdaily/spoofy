// Package settings holds Spoofy's configuration, its file format, and the
// safety rails that keep a long-running traffic generator from becoming an
// incident.
//
// Two design rules govern everything here:
//
//  1. Zero config must work. `spoofy run --url X` with a discoverable spec is a
//     complete invocation. The config file is for when you want more, never a
//     prerequisite for getting started.
//
//  2. The file should read like a sentence an engineer would say. "twenty
//     requests a second, shaped like a normal day" becomes `rate: 20/s` and
//     `shape: diurnal`. Anything requiring a unit conversion in the reader's
//     head is a bug in the format.
//
// Spoofy also runs unattended for weeks, which makes its defaults more
// consequential than a one-shot tool's: a bad value is not a bad run, it is a
// bad month. Defaults err toward refusing to start over doing something
// surprising.
package settings

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults chosen to be immediately useful and safe by accident.
const (
	DefaultRate        = Rate(10)
	DefaultConcurrency = 10
	DefaultTimeout     = Duration(10 * time.Second)
	DefaultMetricsAddr = ":9090"

	// DefaultMaxRate caps sustained throughput unless raised explicitly.
	// Spoofy exists to make an environment look alive, not to load-test it, so
	// a fat-fingered rate should not be able to flatten staging overnight while
	// nobody is watching.
	DefaultMaxRate = Rate(200)
)

// Traffic shape names, as written in config.
const (
	ShapeConstant = "constant"
	ShapeDiurnal  = "diurnal"
	ShapeRamp     = "ramp"
	ShapeSpike    = "spike"
)

// ValidShapes is exported so the CLI can list them in help text and error
// messages without duplicating the list.
var ValidShapes = []string{ShapeConstant, ShapeDiurnal, ShapeRamp, ShapeSpike}

// safeMethods are exercised without opting into writes. Everything else can
// create, mutate, or destroy data, and a daemon doing that unattended for a
// week is a data-loss incident rather than a traffic generator.
var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// Config is the whole of spoofy.yaml, plus a few runtime-only fields set from
// flags rather than file.
type Config struct {
	Target string `yaml:"target"`
	Spec   string `yaml:"spec"`

	Traffic   Traffic        `yaml:"traffic"`
	Endpoints []EndpointRule `yaml:"endpoints"`
	Auth      Auth           `yaml:"auth"`
	Safety    Safety         `yaml:"safety"`
	Metrics   Metrics        `yaml:"metrics"`

	// Runtime-only, never read from file.
	DryRun bool  `yaml:"-"`
	Seed   int64 `yaml:"-"`
	// SourcePath records where this config was loaded from, for error messages.
	SourcePath string `yaml:"-"`
}

// Traffic describes how much load to produce and what shape it takes over time.
//
// Rate is always the *average*. Shapes modulate around it rather than replacing
// it, so changing `shape` never silently changes how much total traffic you
// generate — that is the single most important property of this format, because
// it means a user can try shapes without recalculating anything.
type Traffic struct {
	Rate        Rate     `yaml:"rate"`
	Shape       string   `yaml:"shape"`
	Concurrency int      `yaml:"concurrency"`
	Timeout     Duration `yaml:"timeout"`

	// Amplitude controls how far diurnal traffic swings from the average, as a
	// fraction: 0.6 means the peak is 1.6x the average and the trough 0.4x.
	Amplitude float64 `yaml:"amplitude"`
	// Period is the length of one diurnal cycle. Defaults to 24h.
	Period Duration `yaml:"period"`

	// Ramp: climb from From to To over Over, then hold at To.
	From Rate     `yaml:"from"`
	To   Rate     `yaml:"to"`
	Over Duration `yaml:"over"`

	// Spike: hold Rate, then burst to SpikeRate for SpikeFor, every SpikeEvery.
	SpikeEvery Duration `yaml:"spike_every"`
	SpikeFor   Duration `yaml:"spike_for"`
	SpikeRate  Rate     `yaml:"spike_rate"`
}

// EndpointRule adjusts how a matched set of paths is treated. Rules are
// evaluated in order and the first match wins, like a routing table — the
// alternative (last match wins) makes a config's behaviour depend on how far
// you have read, which is exactly the learning curve this format avoids.
type EndpointRule struct {
	// Match is a glob against the templated path, e.g. "/orders/{id}" or
	// "/admin/*". "*" spans any characters including "/".
	Match string `yaml:"match"`
	// Skip excludes matching endpoints entirely. This is the only way to
	// exclude something — weight is purely a bias.
	Skip bool `yaml:"skip"`
	// Weight biases selection. Unset means 1; 5 means five times as likely.
	//
	// Zero is treated as unset rather than as an exclusion. YAML cannot tell
	// an omitted number from an explicit 0 without a pointer, so making 0 mean
	// "never send this" would silently drop every rule written as a bare
	// `- match: /orders` — a rule whose author plainly meant to include it.
	Weight float64 `yaml:"weight"`
}

// Auth carries credentials applied to every generated request.
type Auth struct {
	Bearer  string            `yaml:"bearer"`
	Basic   BasicAuth         `yaml:"basic"`
	Headers map[string]string `yaml:"headers"`
}

// BasicAuth is HTTP basic credentials.
type BasicAuth struct {
	User string `yaml:"user"`
	Pass string `yaml:"pass"`
}

// Apply attaches configured credentials to req. Explicit headers are applied
// last so they can override the higher-level shorthands.
func (a Auth) Apply(req *http.Request) {
	if a.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+a.Bearer)
	}
	if a.Basic.User != "" || a.Basic.Pass != "" {
		req.SetBasicAuth(a.Basic.User, a.Basic.Pass)
	}
	for k, v := range a.Headers {
		req.Header.Set(k, v)
	}
}

// Safety gathers the rails. They are grouped rather than scattered so that
// reviewing "what could this config do to my environment" is one glance.
type Safety struct {
	AllowWrites bool `yaml:"allow_writes"`
	AllowProd   bool `yaml:"allow_prod"`
	MaxRate     Rate `yaml:"max_rate"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Addr string `yaml:"addr"`
	// Disabled turns the endpoint off. Phrased negatively so that the zero
	// value — an omitted block — leaves metrics on, which is the point of the
	// tool.
	Disabled bool `yaml:"disabled"`
}

// Default returns a Config with everything but target and spec populated.
func Default() Config {
	return Config{
		Traffic: Traffic{
			Rate:        DefaultRate,
			Shape:       ShapeConstant,
			Concurrency: DefaultConcurrency,
			Timeout:     DefaultTimeout,
		},
		Safety:  Safety{MaxRate: DefaultMaxRate},
		Metrics: Metrics{Addr: DefaultMetricsAddr},
	}
}

// ConfigFileNames are searched, in order, when no --config is given.
var ConfigFileNames = []string{"spoofy.yaml", "spoofy.yml", ".spoofy.yaml", ".spoofy.yml"}

// SpecFileNames are searched, in order, when no spec is configured.
var SpecFileNames = []string{
	"openapi.yaml", "openapi.yml", "openapi.json",
	"swagger.yaml", "swagger.yml", "swagger.json",
}

// DiscoverConfig looks for a config file in dir. Empty string means none found,
// which is not an error: running without a config file is a supported mode.
func DiscoverConfig(dir string) string {
	return discover(dir, ConfigFileNames)
}

// DiscoverSpec looks for an OpenAPI document in dir.
func DiscoverSpec(dir string) string {
	return discover(dir, SpecFileNames)
}

func discover(dir string, names []string) string {
	for _, name := range names {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// envRef matches ${NAME} only. Bare $NAME is deliberately left alone: secrets
// and passwords contain "$" often enough that expanding it silently would
// corrupt credentials in ways that are miserable to debug.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces ${NAME} with the environment value, leaving unset
// variables as the empty string. This is what makes a config file safe to
// commit: `bearer: ${API_TOKEN}` keeps the secret out of the repo.
func ExpandEnv(b []byte) []byte {
	return envRef.ReplaceAllFunc(b, func(m []byte) []byte {
		name := string(envRef.FindSubmatch(m)[1])
		return []byte(os.Getenv(name))
	})
}

// Load reads and parses a config file, expanding ${ENV} references. Unknown
// fields are an error: a typo'd key that is silently ignored produces a daemon
// that runs for a week doing the wrong thing, which is the worst outcome this
// package can produce.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(ExpandEnv(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg.SourcePath = path
	cfg.ApplyDefaults()
	return &cfg, nil
}

// ApplyDefaults fills in anything left at its zero value. Safe to call twice.
func (c *Config) ApplyDefaults() {
	if c.Traffic.Rate <= 0 {
		c.Traffic.Rate = DefaultRate
	}
	if c.Traffic.Shape == "" {
		c.Traffic.Shape = ShapeConstant
	}
	if c.Traffic.Concurrency <= 0 {
		c.Traffic.Concurrency = DefaultConcurrency
	}
	if c.Traffic.Timeout <= 0 {
		c.Traffic.Timeout = DefaultTimeout
	}
	if c.Safety.MaxRate <= 0 {
		c.Safety.MaxRate = DefaultMaxRate
	}
	if c.Metrics.Addr == "" {
		c.Metrics.Addr = DefaultMetricsAddr
	}
}

// Validate reports every problem at once rather than making the user rediscover
// them one restart at a time.
func (c *Config) Validate() error {
	var errs []error

	errs = append(errs, c.validateTarget()...)

	if c.Spec == "" {
		errs = append(errs, errors.New(
			"no OpenAPI spec configured or discovered; set `spec:` in config or pass --spec"))
	}

	errs = append(errs, c.validateTraffic()...)

	for i, rule := range c.Endpoints {
		if rule.Match == "" {
			errs = append(errs, fmt.Errorf("endpoints[%d]: `match` is required", i))
		}
		if rule.Weight < 0 {
			errs = append(errs, fmt.Errorf("endpoints[%d]: weight must not be negative, got %g", i, rule.Weight))
		}
	}

	return errors.Join(errs...)
}

func (c *Config) validateTarget() []error {
	if c.Target == "" {
		return []error{errors.New("target URL is required (set `target:` in config or pass --url)")}
	}

	u, err := url.Parse(c.Target)
	switch {
	case err != nil:
		return []error{fmt.Errorf("target %q is not a valid URL: %w", c.Target, err)}
	case u.Scheme != "http" && u.Scheme != "https":
		return []error{fmt.Errorf("target %q must use http or https, got scheme %q", c.Target, u.Scheme)}
	case u.Host == "":
		return []error{fmt.Errorf("target %q has no host", c.Target)}
	case LooksLikeProduction(u.Host) && !c.Safety.AllowProd:
		return []error{fmt.Errorf(
			"target %q looks like production and Spoofy refuses to generate traffic against it.\n"+
				"  If this really is a non-production environment, set safety.allow_prod: true (or pass --allow-prod)",
			u.Host)}
	}
	return nil
}

func (c *Config) validateTraffic() []error {
	var errs []error
	t := c.Traffic

	if t.Rate <= 0 {
		errs = append(errs, fmt.Errorf("traffic.rate must be positive, got %s", t.Rate))
	}
	if t.Rate > c.Safety.MaxRate {
		errs = append(errs, fmt.Errorf(
			"traffic.rate %s exceeds the safety ceiling of %s.\n"+
				"  Raise safety.max_rate if this is deliberate — the ceiling exists so a typo cannot flatten an environment",
			t.Rate, c.Safety.MaxRate))
	}
	if t.Concurrency <= 0 {
		errs = append(errs, fmt.Errorf("traffic.concurrency must be positive, got %d", t.Concurrency))
	}
	if t.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("traffic.timeout must be positive, got %s", t.Timeout))
	}

	if !isValidShape(t.Shape) {
		errs = append(errs, fmt.Errorf(
			"traffic.shape %q is not recognised; valid shapes are %s", t.Shape, strings.Join(ValidShapes, ", ")))
		return errs
	}

	// Shape-specific requirements. Each error names the exact keys to add,
	// because "invalid ramp config" tells the reader nothing actionable.
	switch t.Shape {
	case ShapeDiurnal:
		if t.Amplitude < 0 || t.Amplitude > 1 {
			errs = append(errs, fmt.Errorf(
				"traffic.amplitude must be between 0 and 1, got %g (0.6 means peaks 1.6x the average)", t.Amplitude))
		}
	case ShapeRamp:
		if t.Over <= 0 {
			errs = append(errs, errors.New("traffic.shape: ramp requires `over` (e.g. over: 30m)"))
		}
		if t.To <= 0 {
			errs = append(errs, errors.New("traffic.shape: ramp requires `to` (e.g. to: 50/s)"))
		}
		if t.To > c.Safety.MaxRate {
			errs = append(errs, fmt.Errorf("traffic.to %s exceeds the safety ceiling of %s", t.To, c.Safety.MaxRate))
		}
	case ShapeSpike:
		if t.SpikeEvery <= 0 {
			errs = append(errs, errors.New("traffic.shape: spike requires `spike_every` (e.g. spike_every: 30m)"))
		}
		if t.SpikeFor <= 0 {
			errs = append(errs, errors.New("traffic.shape: spike requires `spike_for` (e.g. spike_for: 2m)"))
		}
		if t.SpikeRate <= 0 {
			errs = append(errs, errors.New("traffic.shape: spike requires `spike_rate` (e.g. spike_rate: 100/s)"))
		}
		if t.SpikeRate > c.Safety.MaxRate {
			errs = append(errs, fmt.Errorf(
				"traffic.spike_rate %s exceeds the safety ceiling of %s", t.SpikeRate, c.Safety.MaxRate))
		}
		if t.SpikeFor > 0 && t.SpikeEvery > 0 && t.SpikeFor >= t.SpikeEvery {
			errs = append(errs, fmt.Errorf(
				"traffic.spike_for (%s) must be shorter than spike_every (%s), otherwise the spike never ends",
				t.SpikeFor, t.SpikeEvery))
		}
	}

	return errs
}

func isValidShape(s string) bool {
	for _, v := range ValidShapes {
		if s == v {
			return true
		}
	}
	return false
}

// MethodAllowed reports whether the daemon may exercise this HTTP method.
func (c *Config) MethodAllowed(method string) bool {
	if safeMethods[strings.ToUpper(method)] {
		return true
	}
	return c.Safety.AllowWrites
}

// RuleFor returns the first endpoint rule matching a templated path, or nil.
func (c *Config) RuleFor(templatedPath string) *EndpointRule {
	for i := range c.Endpoints {
		if matchGlob(c.Endpoints[i].Match, templatedPath) {
			return &c.Endpoints[i]
		}
	}
	return nil
}

// PathAllowed reports whether a templated path should be exercised at all.
func (c *Config) PathAllowed(templatedPath string) bool {
	rule := c.RuleFor(templatedPath)
	if rule == nil {
		return true
	}
	return !rule.Skip
}

// WeightFor returns the selection weight for a templated path, defaulting to 1.
func (c *Config) WeightFor(templatedPath string) float64 {
	rule := c.RuleFor(templatedPath)
	if rule == nil || rule.Weight <= 0 {
		return 1
	}
	return rule.Weight
}

// productionTokens mark a host as production. Matched against tokens split on
// ".", "-", and "_" so that "api-prod.acme.com" matches while "prodigy.com"
// does not.
var productionTokens = map[string]bool{
	"prod": true, "production": true, "prd": true,
}

// nonProductionTokens are an explicit override: a host carrying one of these is
// treated as safe even if it also carries a production token, because
// "staging-prod-mirror" is a staging box.
var nonProductionTokens = map[string]bool{
	"staging": true, "stage": true, "stg": true, "dev": true, "devel": true,
	"development": true, "test": true, "testing": true, "qa": true, "uat": true,
	"sandbox": true, "sbx": true, "local": true, "localhost": true, "demo": true,
}

// LooksLikeProduction applies a deliberately conservative heuristic to a host.
//
// It refuses only on an explicit production marker, and never infers from the
// absence of one. Guessing would reject legitimate targets like a bare internal
// hostname, and a safety rail that fires during correct usage is a safety rail
// people switch off permanently.
func LooksLikeProduction(host string) bool {
	if h, _, ok := strings.Cut(host, ":"); ok && !strings.Contains(host, "]") {
		host = h
	}
	tokens := strings.FieldsFunc(strings.ToLower(host), func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	})

	var hasProd bool
	for _, t := range tokens {
		if nonProductionTokens[t] {
			return false
		}
		if productionTokens[t] {
			hasProd = true
		}
	}
	return hasProd
}

// matchGlob matches a pattern where "*" spans any run of characters, including
// "/". A user writing "/admin/*" means everything beneath /admin, which is not
// what path.Match does — hence the hand-rolled matcher.
func matchGlob(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}

	parts := strings.Split(pattern, "*")

	if head := parts[0]; head != "" {
		if !strings.HasPrefix(s, head) {
			return false
		}
		s = s[len(head):]
	}
	if tail := parts[len(parts)-1]; tail != "" {
		if !strings.HasSuffix(s, tail) {
			return false
		}
		s = s[:len(s)-len(tail)]
	}
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return true
}
