// Package cli defines Spoofy's command-line surface.
//
// The constraint shaping it is that a first run must work without a config
// file. `spoofy run --url http://localhost:8080` with a discoverable spec is a
// complete invocation; everything else is progressive disclosure.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ashdaily/spoofy/internal/engine"
	"github.com/ashdaily/spoofy/internal/metrics"
	"github.com/ashdaily/spoofy/internal/openapi"
	"github.com/ashdaily/spoofy/internal/report"
	"github.com/ashdaily/spoofy/internal/settings"
	"github.com/ashdaily/spoofy/internal/version"
)

// ExecuteContext runs the CLI and returns a process exit code.
func ExecuteContext(ctx context.Context) int {
	root := newRootCmd()
	if err := root.ExecuteContext(ctx); err != nil {
		// Formatted for a human at a terminal rather than as a Go error chain.
		// Most are configuration mistakes, and the fix should be visible
		// without reading source.
		fmt.Fprintf(os.Stderr, "\nerror: %v\n\n", err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "spoofy",
		Short: "Keep non-production environments full of realistic traffic",
		Long: strings.TrimSpace(`
Spoofy reads an OpenAPI spec and generates continuous, production-shaped
traffic against a non-production environment, so dashboards have signal and
alert thresholds can be tuned against something other than a flat line.

It is a daemon, not a test run: it keeps going until you stop it.`),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newRunCmd(), newInitCmd(), newVersionCmd())
	return root
}

// runFlags holds the flag-bound values before they are merged over config.
type runFlags struct {
	configPath  string
	spec        string
	url         string
	rate        string
	shape       string
	concurrency int
	timeout     time.Duration
	duration    time.Duration
	allowWrites bool
	allowProd   bool
	dryRun      bool
	metricsAddr string
	noMetrics   bool
	only        []string
	skip        []string
	seed        int64
	bearer      string
	basic       string
	headers     []string
}

func newRunCmd() *cobra.Command {
	var f runFlags

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Generate traffic until stopped",
		Example: strings.TrimSpace(`
  # Simplest possible run: spec discovered in the current directory
  spoofy run --url http://localhost:8080

  # Shaped like a working day, averaging 20 requests a second
  spoofy run --url http://localhost:8080 --rate 20/s --shape diurnal

  # See exactly what would be sent, without sending it
  spoofy run --url http://localhost:8080 --dry-run

  # Everything in a file
  spoofy run --config spoofy.yaml`),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTraffic(cmd)
		},
	}

	fl := cmd.Flags()
	fl.StringVarP(&f.configPath, "config", "c", "", "config file (default: spoofy.yaml if present)")
	fl.StringVarP(&f.spec, "spec", "s", "", "OpenAPI spec: file path or URL (default: discovered)")
	fl.StringVarP(&f.url, "url", "u", "", "target base URL, e.g. http://localhost:8080")
	fl.StringVarP(&f.rate, "rate", "r", "", "average request rate, e.g. 20/s or 1200/m")
	fl.StringVar(&f.shape, "shape", "", "traffic shape: "+strings.Join(settings.ValidShapes, ", "))
	fl.IntVar(&f.concurrency, "concurrency", 0, "requests in flight at once")
	fl.DurationVar(&f.timeout, "timeout", 0, "per-request timeout")
	fl.DurationVar(&f.duration, "duration", 0, "stop after this long (default: run forever)")
	fl.BoolVar(&f.allowWrites, "allow-writes", false, "exercise POST/PUT/PATCH/DELETE as well as reads")
	fl.BoolVar(&f.allowProd, "allow-prod", false, "permit a target whose hostname looks like production")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the requests that would be sent, then exit")
	fl.StringVar(&f.metricsAddr, "metrics-addr", "", "address for the Prometheus endpoint")
	fl.BoolVar(&f.noMetrics, "no-metrics", false, "do not serve Prometheus metrics")
	fl.StringSliceVar(&f.only, "only", nil, "only exercise paths matching these globs")
	fl.StringSliceVar(&f.skip, "skip", nil, "never exercise paths matching these globs")
	fl.Int64Var(&f.seed, "seed", 0, "seed for reproducible runs")
	fl.Duration("startup-timeout", DefaultStartupTimeout, "how long to wait for a remote spec to become reachable")
	fl.StringVar(&f.bearer, "auth-bearer", "", "bearer token")
	fl.StringVar(&f.basic, "auth-basic", "", "basic credentials as user:pass")
	fl.StringArrayVar(&f.headers, "header", nil, "extra header as 'Name: value' (repeatable)")

	return cmd
}

// resolve merges config file and flags. Flags win, since someone typing one is
// being more specific than a file committed months ago.
//
// Values come from the flag set rather than a parallel struct, so a flag name
// is spelled in exactly one place.
func resolve(cmd *cobra.Command) (*settings.Config, error) {
	fl := cmd.Flags()
	str := func(name string) string { v, _ := fl.GetString(name); return v }

	cfg := settings.Default()

	path := str("config")
	if path == "" {
		path = settings.DiscoverConfig(".")
	}
	if path != "" {
		loaded, err := settings.Load(path)
		if err != nil {
			return nil, err
		}
		cfg = *loaded
	}

	changed := func(name string) bool { return fl.Changed(name) }

	if changed("url") {
		cfg.Target = str("url")
	}
	if changed("spec") {
		cfg.Spec = str("spec")
	}
	if changed("rate") {
		r, err := settings.ParseRate(str("rate"))
		if err != nil {
			return nil, err
		}
		cfg.Traffic.Rate = r
	}
	if changed("shape") {
		cfg.Traffic.Shape = str("shape")
	}
	if changed("concurrency") {
		v, _ := fl.GetInt("concurrency")
		cfg.Traffic.Concurrency = v
	}
	if changed("timeout") {
		v, _ := fl.GetDuration("timeout")
		cfg.Traffic.Timeout = settings.Duration(v)
	}
	if changed("allow-writes") {
		v, _ := fl.GetBool("allow-writes")
		cfg.Safety.AllowWrites = v
	}
	if changed("allow-prod") {
		v, _ := fl.GetBool("allow-prod")
		cfg.Safety.AllowProd = v
	}
	if changed("metrics-addr") {
		cfg.Metrics.Addr = str("metrics-addr")
	}
	if changed("no-metrics") {
		v, _ := fl.GetBool("no-metrics")
		cfg.Metrics.Disabled = v
	}
	if changed("seed") {
		v, _ := fl.GetInt64("seed")
		cfg.Seed = v
	}
	if changed("auth-bearer") {
		cfg.Auth.Bearer = str("auth-bearer")
	}
	if changed("auth-basic") {
		raw := str("auth-basic")
		user, pass, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, fmt.Errorf("--auth-basic must be user:pass, got %q", raw)
		}
		cfg.Auth.Basic = settings.BasicAuth{User: user, Pass: pass}
	}
	headers, _ := fl.GetStringArray("header")
	for _, h := range headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("--header must be 'Name: value', got %q", h)
		}
		if cfg.Auth.Headers == nil {
			cfg.Auth.Headers = map[string]string{}
		}
		cfg.Auth.Headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}

	// --only and --skip append rules, so they compose with a config file instead
	// of replacing what it declares.
	skip, _ := fl.GetStringSlice("skip")
	only, _ := fl.GetStringSlice("only")
	for _, pattern := range skip {
		cfg.Endpoints = append(cfg.Endpoints, settings.EndpointRule{Match: pattern, Skip: true})
	}
	if len(only) > 0 {
		for _, pattern := range only {
			cfg.Endpoints = append(cfg.Endpoints, settings.EndpointRule{Match: pattern})
		}
		// Anything not named is excluded.
		cfg.Endpoints = append(cfg.Endpoints, settings.EndpointRule{Match: "*", Skip: true})
	}

	cfg.DryRun, _ = fl.GetBool("dry-run")

	if cfg.Spec == "" {
		if discovered := settings.DiscoverSpec("."); discovered != "" {
			cfg.Spec = discovered
		}
	}

	cfg.ApplyDefaults()
	return &cfg, nil
}

func runTraffic(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	cfg, err := resolve(cmd)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	spec, err := loadSpec(ctx, out, cfg.Spec, startupTimeout(cmd))
	if err != nil {
		return err
	}

	selected, skippedByMethod, skippedByPath := spec.Select(cfg)
	if len(selected) == 0 {
		return fmt.Errorf(
			"every operation was filtered out: %d excluded because writes are disabled, %d by endpoint rules.\n"+
				"  Pass --allow-writes to include POST/PUT/PATCH/DELETE, or relax your --only/--skip patterns",
			skippedByMethod, skippedByPath)
	}

	mx := metrics.New()
	eng, err := engine.New(cfg, selected, mx, time.Now())
	if err != nil {
		return err
	}

	printBanner(out, cfg, spec, eng, skippedByMethod, skippedByPath)

	if cfg.DryRun {
		return printDryRun(ctx, out, eng)
	}

	// SIGTERM from Kubernetes, SIGINT from ctrl-c. Both mean drain.
	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	runCtx := sigCtx
	if duration, _ := cmd.Flags().GetDuration("duration"); duration > 0 {
		var stopTimer context.CancelFunc
		runCtx, stopTimer = context.WithTimeout(sigCtx, duration)
		defer stopTimer()
	}

	if !cfg.Metrics.Disabled {
		go func() {
			if err := mx.Serve(ctx, cfg.Metrics.Addr); err != nil {
				fmt.Fprintf(os.Stderr, "metrics endpoint stopped: %v\n", err)
			}
		}()
	}

	live := report.NewLive(out, cfg.Target, eng.Scheduler().Shape().Describe(), 500*time.Millisecond)
	liveDone := make(chan struct{})
	go live.Run(liveDone, func() (engine.Snapshot, float64, bool) {
		return eng.Stats().Snapshot(time.Now()), eng.Scheduler().CurrentRate(), eng.Client().Up()
	})

	eng.Run(runCtx)
	close(liveDone)

	// Let the final frame land before the summary.
	time.Sleep(50 * time.Millisecond)
	printSummary(out, eng)

	if err := runCtx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func printBanner(out io.Writer, cfg *settings.Config, spec *openapi.Spec, eng *engine.Engine, skippedByMethod, skippedByPath int) {
	fmt.Fprintf(out, "\n  spoofy %s\n\n", version.Version)
	fmt.Fprintf(out, "  spec      %s", cfg.Spec)
	if spec.Title != "" {
		fmt.Fprintf(out, "  (%s %s)", spec.Title, spec.Version)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  target    %s\n", cfg.Target)
	fmt.Fprintf(out, "  traffic   %s\n", eng.Scheduler().Shape().Describe())
	fmt.Fprintf(out, "  workers   %d concurrent\n", cfg.Traffic.Concurrency)

	// State how much of the spec is being exercised, so a run that drops most of
	// it cannot pass for a healthy one.
	fmt.Fprintf(out, "  endpoints %d of %d", len(eng.Operations()), len(spec.Operations))
	var reasons []string
	if skippedByMethod > 0 {
		reasons = append(reasons, fmt.Sprintf("%d writes (use --allow-writes)", skippedByMethod))
	}
	if skippedByPath > 0 {
		reasons = append(reasons, fmt.Sprintf("%d filtered", skippedByPath))
	}
	if len(reasons) > 0 {
		fmt.Fprintf(out, ", skipped %s", strings.Join(reasons, ", "))
	}
	fmt.Fprintln(out)

	if !cfg.Metrics.Disabled {
		fmt.Fprintf(out, "  metrics   http://localhost%s/metrics\n", cfg.Metrics.Addr)
	}
	if !cfg.Safety.AllowWrites {
		fmt.Fprintf(out, "  safety    read-only (GET, HEAD, OPTIONS)\n")
	} else {
		fmt.Fprintf(out, "  safety    WRITES ENABLED: this run can create and delete data\n")
	}
	fmt.Fprintln(out)
}

func printDryRun(ctx context.Context, out io.Writer, eng *engine.Engine) error {
	commands, err := eng.DryRun(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  Dry run: %d request(s), nothing sent.\n\n", len(commands))
	for i, c := range commands {
		fmt.Fprintf(out, "  %s\n\n", eng.Operations()[i])
		fmt.Fprintf(out, "%s\n\n", c)
	}
	return nil
}

func printSummary(out io.Writer, eng *engine.Engine) {
	s := eng.Stats().Snapshot(time.Now())
	fmt.Fprintf(out, "\n  stopped after %s: %d requests, %.1f%% ok\n\n",
		s.Elapsed.Round(time.Second), s.Total, s.SuccessRate()*100)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}
