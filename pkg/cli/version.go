package cli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

func newVersionCommand(info BuildInfo) *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "Print version information",
		Action: func(_ context.Context, c *cli.Command) error {
			_, err := fmt.Fprintf(c.Root().Writer, "klotho %s (%s, %s)\n", info.Version, info.Commit, info.Date)
			return err
		},
	}
}
