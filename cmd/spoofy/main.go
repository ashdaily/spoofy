// Command spoofy generates continuous, production-shaped traffic against a
// non-production environment from an OpenAPI spec.
package main

import (
	"context"
	"os"

	"github.com/ashdaily/spoofy/internal/cli"
)

func main() {
	// The root context lives here rather than inside the CLI so that signal
	// handling and shutdown ordering stay visible at the top level.
	os.Exit(cli.ExecuteContext(context.Background()))
}
