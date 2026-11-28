package cli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
)

func newUpgradeCommand() *cli.Command {
	return &cli.Command{
		Name:      "upgrade",
		Usage:     "Upgrade a cluster's Kubernetes version",
		ArgsUsage: "[name]",
		Description: "Raises the Kubernetes version through the cluster's own Cluster API: it\n" +
			"patches the control plane object's spec.version and lets the controller roll\n" +
			"the control plane machines, then patches each MachineDeployment. It is pure,\n" +
			"provider-agnostic Cluster API orchestration and holds no workload opinion.\n\n" +
			"With --watch klotho follows the control plane's rolling replace to completion\n" +
			"(new machine joined, old removed, quorum preserved) before touching the\n" +
			"workers, honoring the Cluster API one-minor skew. Without it klotho patches\n" +
			"and returns, leaving the controllers to roll at their own pace.\n\n" +
			"klotho refuses a jump across more than one minor, a downgrade, and workers\n" +
			"ahead of the control plane; it surfaces each as a finding and never edits the\n" +
			"cluster to resolve one. --force applies the change anyway.\n\n" +
			"A plain image that pins the Kubernetes version at boot (klotho's\n" +
			"install-at-boot script, a prepared Proxmox template) cannot derive the target\n" +
			"version from the Machine spec; point the machines at a new image for the\n" +
			"target version with --control-plane-image / --worker-image. CAPD needs\n" +
			"neither: its DockerMachine derives kindest/node:v<version> from the Machine.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "kubernetes-version",
				Aliases:  []string{"k"},
				Usage:    "target Kubernetes version, e.g. v1.33.4",
				Required: true,
			},
			&cli.StringFlag{
				Name:  "cluster",
				Usage: "cluster to upgrade (alternative to the positional name argument)",
			},
			&cli.BoolFlag{
				Name:  "control-plane-only",
				Usage: "upgrade only the control plane",
			},
			&cli.BoolFlag{
				Name:  "workers-only",
				Usage: "upgrade only the MachineDeployments (requires the control plane already at or above the target)",
			},
			&cli.BoolFlag{
				Name:  "watch",
				Usage: "follow each rollout to completion before moving on (control plane, then workers)",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "apply the version change despite skew findings (printed either way)",
			},
			&cli.DurationFlag{
				Name:  "watch-timeout",
				Usage: "with --watch, how long to follow each rollout before giving up (default 20m)",
			},
			&cli.StringFlag{
				Name:  "control-plane-image",
				Usage: "point the control plane at this node image for the target version (template-swap path for version-pinned images)",
			},
			&cli.StringFlag{
				Name:  "worker-image",
				Usage: "point every worker pool at this node image for the target version",
			},
			&cli.StringFlag{
				Name:  "image-field",
				Usage: "field under the infrastructure MachineTemplate's spec.template.spec that carries the image (default customImage)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			name, err := upgradeClusterName(c)
			if err != nil {
				return err
			}
			a, err := newApp(c)
			if err != nil {
				return err
			}
			opts := klotho.UpgradeOptions{
				KubernetesVersion: c.String("kubernetes-version"),
				ControlPlaneOnly:  c.Bool("control-plane-only"),
				WorkersOnly:       c.Bool("workers-only"),
				Watch:             c.Bool("watch"),
				Force:             c.Bool("force"),
				WatchTimeout:      c.Duration("watch-timeout"),
				ControlPlaneImage: c.String("control-plane-image"),
				WorkerImage:       c.String("worker-image"),
				ImageField:        c.String("image-field"),
			}
			if err := klotho.Upgrade(ctx, a.deps(), name, opts); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout(c), "upgraded %s to %s\n", name, c.String("kubernetes-version"))
			return nil
		},
	}
}

// upgradeClusterName resolves the cluster name from the positional argument or
// the --cluster flag, validating it. Exactly one must be given.
func upgradeClusterName(c *cli.Command) (string, error) {
	name := c.String("cluster")
	if arg := c.Args().First(); arg != "" {
		if name != "" && name != arg {
			return "", fmt.Errorf("upgrade: cluster named twice: %q and --cluster %q", arg, name)
		}
		name = arg
	}
	if name == "" {
		return "", fmt.Errorf("upgrade: a cluster name is required (as an argument or --cluster)")
	}
	if err := klotho.ValidateName(name); err != nil {
		return "", fmt.Errorf("upgrade: %w", err)
	}
	return name, nil
}
