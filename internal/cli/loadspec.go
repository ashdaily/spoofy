package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ashdaily/spoofy/internal/openapi"
)

// DefaultStartupTimeout bounds how long Spoofy waits for a remote spec to
// become reachable before giving up.
const DefaultStartupTimeout = 60 * time.Second

func startupTimeout(cmd *cobra.Command) time.Duration {
	if d, err := cmd.Flags().GetDuration("startup-timeout"); err == nil && d > 0 {
		return d
	}
	return DefaultStartupTimeout
}

// loadSpec loads the spec, retrying while a remote one is still coming up.
//
// Under Docker Compose and Kubernetes everything starts at once, so the API is
// routinely unreachable for the first few seconds. Exiting on that turns
// ordinary startup ordering into a crash loop. A local file gets no retry,
// since a missing path is a mistake waiting will not fix.
func loadSpec(ctx context.Context, out io.Writer, source string, timeout time.Duration) (*openapi.Spec, error) {
	spec, err := openapi.Load(ctx, source)
	if err == nil || !isRemote(source) {
		return spec, err
	}

	deadline := time.Now().Add(timeout)
	fmt.Fprintf(out, "  waiting for %s (up to %s)...\n", source, timeout)

	backoff := 500 * time.Millisecond
	for attempt := 2; time.Now().Before(deadline); attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		spec, err = openapi.Load(ctx, source)
		if err == nil {
			fmt.Fprintf(out, "  spec available after %d attempts\n\n", attempt)
			return spec, nil
		}

		if backoff < 5*time.Second {
			backoff *= 2
		}
	}

	return nil, fmt.Errorf(
		"spec at %s was still unreachable after %s: %w\n"+
			"  Raise --startup-timeout if the API takes longer to come up",
		source, timeout, err)
}

func isRemote(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}
