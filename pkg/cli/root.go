// Package cli defines the klotho command-line interface.
package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
)

// BuildInfo describes the build that produced the running binary.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// ErrNotImplemented is returned for options whose implementation has not
// landed yet, such as providers other than the ones klotho ships.
var ErrNotImplemented = errors.New("not implemented")

// New returns the root klotho command.
func New(info BuildInfo) *cli.Command {
	return &cli.Command{
		Name:    "klotho",
		Usage:   "Bootstrap self-managed Cluster API clusters",
		Version: info.Version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "log-level",
				Usage: "log verbosity (debug, info, warn, error)",
				Value: "info",
			},
			&cli.StringFlag{
				Name:    "backend",
				Usage:   "state backend to use (proxmox, file)",
				Sources: cli.EnvVars("KLOTHO_BACKEND"),
			},
			&cli.StringFlag{
				Name:    "config",
				Usage:   "configuration file (default $XDG_CONFIG_HOME/klotho/config.yaml)",
				Sources: cli.EnvVars("KLOTHO_CONFIG"),
			},
			&cli.StringFlag{
				Name:    "profile",
				Usage:   "configuration profile to merge over the top level",
				Sources: cli.EnvVars("KLOTHO_PROFILE"),
			},
		},
		Commands: []*cli.Command{
			newInitCommand(),
			newCreateCommand(),
			newDeleteCommand(),
			newListCommand(),
			newShellCommand(),
			newStateCommand(),
			newCredentialCommand(),
			newProbeCommand(),
			newHyperVAgentCommand(),
			newVersionCommand(info),
		},
	}
}

// Run builds the root command and executes it with the given arguments.
func Run(ctx context.Context, args []string, info BuildInfo) error {
	return New(info).Run(ctx, args)
}

// clusterNameArg returns the single required cluster name argument or an error.
func clusterNameArg(c *cli.Command) (string, error) {
	if c.Args().Len() < 1 || c.Args().First() == "" {
		return "", fmt.Errorf("%s: a cluster name is required", c.Name)
	}
	name := c.Args().First()
	if err := klotho.ValidateName(name); err != nil {
		return "", fmt.Errorf("%s: %w", c.Name, err)
	}
	return name, nil
}
