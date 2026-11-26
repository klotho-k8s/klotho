package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho/pkg/config"
)

func newInitCommand() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "Create the configuration directory and the age identity that encrypts cluster state",
		Description: "Writes a commented config.yaml if none exists and an age identity file (mode 0600)\n" +
			"if none exists, then prints the identity's public key. Running it again changes nothing.\n" +
			"Share the public key with anyone who should be able to read state you write\n" +
			"(encryption.recipients), and keep the identity file private.",
		Action: func(_ context.Context, c *cli.Command) error {
			path := c.Root().String("config")
			if path == "" {
				path = config.DefaultPath()
			}
			path = config.Expand(path)
			wrote, err := config.WriteSkeleton(path)
			if err != nil {
				return err
			}
			// Load after writing so the identity path honors the file, the
			// profile and the environment.
			cfg, err := config.Load(path, c.Root().String("profile"))
			if err != nil {
				return err
			}
			if cfg.Encryption.Plain {
				fmt.Fprintln(stderr(c), "warning: encryption.plain is set; state will be stored unencrypted")
			}
			key, err := config.Init(cfg.Encryption.IdentityFile)
			if err != nil {
				return err
			}
			w := stdout(c)
			state := "existing"
			if wrote {
				state = "new"
			}
			fmt.Fprintf(w, "config:     %s (%s)\n", filepath.Clean(path), state)
			fmt.Fprintf(w, "identity:   %s\n", cfg.Encryption.IdentityFile)
			fmt.Fprintf(w, "state dir:  %s\n", cfg.StateDir)
			fmt.Fprintf(w, "backend:    %s\n", cfg.EffectiveBackend())
			fmt.Fprintf(w, "public key: %s\n", key)
			return nil
		},
	}
}
