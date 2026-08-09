package main

import (
	"context"
	"os"

	"github.com/ashdaily/spoofy/internal/cli"
)

func main() {
	os.Exit(cli.ExecuteContext(context.Background()))
}
