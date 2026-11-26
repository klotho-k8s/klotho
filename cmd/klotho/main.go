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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	info := cli.BuildInfo{Version: version, Commit: commit, Date: date}
	if err := cli.Run(ctx, os.Args, info); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		stop()
		os.Exit(1)
	}
}
