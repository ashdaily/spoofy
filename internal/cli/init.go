package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ashdaily/spoofy/internal/settings"
)

// starterConfig is what `spoofy init` writes.
//
// It is heavily commented on purpose. The config file is the main thing a new
// user has to learn, and a generated file that explains itself is worth more
// than a documentation page they have to find. Every option is shown but only
// the two required ones are uncommented, so the file works immediately and
// grows by deleting `#` rather than by looking things up.
const starterConfig = `# spoofy.yaml — continuous traffic for a non-production environment.
#
# Only 'target' and 'spec' are required. Everything below is optional and
# shown at its default. Uncomment what you need.

target: http://localhost:8080
spec: ./openapi.yaml

traffic:
  # Average requests per second. Write it however you say it out loud:
  # "20/s", "1200/m", "72000/h" all mean the same thing.
  rate: 10/s

  # How traffic varies over time. 'rate' above is always the AVERAGE, so
  # changing shape redistributes traffic without changing the total.
  #
  #   constant  steady, predictable. The default.
  #   diurnal   busy afternoons, quiet nights. Looks like a real service.
  #   ramp      climb from one rate to another, then hold.
  #   spike     a baseline with periodic bursts. Good for tripping alerts.
  shape: constant

  # Requests in flight at once.
  # concurrency: 10

  # Per-request timeout.
  # timeout: 10s

  # --- diurnal only ---
  # How far traffic swings from the average. 0.6 means the afternoon peak is
  # 1.6x the average and the small hours are 0.4x.
  # amplitude: 0.6
  # period: 24h

  # --- ramp only ---
  # from: 5/s
  # to: 50/s
  # over: 30m

  # --- spike only ---
  # spike_every: 30m
  # spike_for: 2m
  # spike_rate: 100/s

# Bias or exclude specific endpoints. Rules are checked in order and the first
# match wins. '*' matches any characters, including '/'.
# endpoints:
#   - match: /admin/*
#     skip: true
#   - match: /health
#     skip: true
#   - match: /orders
#     weight: 5        # hit five times as often as everything else

# ${VARS} are read from the environment at startup, so this file is safe to
# commit with secrets referenced rather than embedded.
# auth:
#   bearer: ${API_TOKEN}
#   basic:
#     user: ${API_USER}
#     pass: ${API_PASS}
#   headers:
#     X-Tenant: acme

safety:
  # Spoofy only sends GET, HEAD, and OPTIONS unless you turn this on.
  # A daemon quietly POSTing generated rows into staging for a week is a
  # data-loss incident, not a traffic generator.
  allow_writes: false

  # Refuse targets whose hostname looks like production.
  # allow_prod: false

  # Hard ceiling on rate, so a typo cannot flatten an environment overnight.
  # max_rate: 200/s

# metrics:
#   addr: ":9090"
#   disabled: false
`

func newInitCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Write a starter spoofy.yaml",
		Long: "Writes a commented configuration file covering every option, with only the\n" +
			"required ones active. Edit it by deleting '#' rather than by looking things up.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "spoofy.yaml"
			if len(args) == 1 {
				path = args[0]
			}

			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", path)
			}

			if dir := filepath.Dir(path); dir != "." {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("creating %s: %w", dir, err)
				}
			}

			if err := os.WriteFile(path, []byte(starterConfig), 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", path, err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "\n  Wrote %s\n\n", path)

			if spec := settings.DiscoverSpec("."); spec != "" {
				fmt.Fprintf(out, "  Found a spec at %s — set it as `spec:` if that is the one you want.\n", spec)
			} else {
				fmt.Fprintf(out, "  No OpenAPI spec found here. Point `spec:` at yours, or at a URL\n")
				fmt.Fprintf(out, "  like http://localhost:8080/openapi.json.\n")
			}
			fmt.Fprintf(out, "\n  Then:  spoofy run --dry-run    # see what it would send\n")
			fmt.Fprintf(out, "         spoofy run                # start generating traffic\n\n")
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite an existing file")
	return cmd
}
