// Command klotho bootstraps self-managed Cluster API clusters.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/klotho-k8s/klotho/pkg/cli"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	info := cli.BuildInfo{Version: version, Commit: commit, Date: date}
	if err := cli.Run(ctx, os.Args, info); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
