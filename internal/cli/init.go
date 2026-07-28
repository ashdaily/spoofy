package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ashdaily/spoofy/internal/settings"
)

// starterConfig is what `spoofy init` writes: a working file, not a commented
// catalogue of every option.
//
// A generated config gets edited and committed, so commented-out alternatives
// become noise in every diff afterwards. The option reference lives in the
// readme instead.
const starterConfig = `# spoofy.yaml. Full option reference:
# https://github.com/ashdaily/spoofy#configuration

target: http://localhost:8080
spec: ./openapi.yaml

traffic:
  rate: 10/s
  shape: constant
  concurrency: 10
  timeout: 10s

safety:
  allow_writes: false
  allow_prod: false
  max_rate: 200/s

metrics:
  addr: ":9090"
`

func newInitCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Write a starter spoofy.yaml",
		Long: "Writes a working configuration file with sensible defaults.\n\n" +
			"Traffic shapes, endpoint weighting, auth, and the full option reference are\n" +
			"documented at https://github.com/ashdaily/spoofy#configuration",
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
				fmt.Fprintf(out, "  Found a spec at %s. Set it as `spec:` if that is the one you want.\n", spec)
			} else {
				fmt.Fprintf(out, "  No OpenAPI spec found here. Point `spec:` at yours, or at a URL\n")
				fmt.Fprintf(out, "  like http://localhost:8080/openapi.json.\n")
			}
			fmt.Fprintf(out, "\n  Then:  spoofy run --dry-run    # see what it would send\n")
			fmt.Fprintf(out, "         spoofy run                # start generating traffic\n")
			fmt.Fprintf(out, "\n  Traffic shapes, endpoint weights, and auth:\n")
			fmt.Fprintf(out, "  https://github.com/ashdaily/spoofy#configuration\n\n")
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite an existing file")
	return cmd
}
