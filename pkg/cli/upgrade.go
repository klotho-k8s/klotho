package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
)

func newUpgradeCommand() *cli.Command {
	return &cli.Command{
		Name:      "upgrade",
		Usage:     "Upgrade a cluster's Kubernetes version and/or its Cluster API components",
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
			"neither: its DockerMachine derives kindest/node:v<version> from the Machine.\n\n" +
			"Day-2 management-plane upgrade: --capi-version bumps the Cluster API core and\n" +
			"kubeadm bootstrap/control-plane providers together, --provider-version the\n" +
			"infrastructure provider; the matching --*-image flags carry a fork or mirror\n" +
			"image. klotho re-applies the operator provider CRs at the target version and\n" +
			"waits for each to roll out (core, bootstrap, control-plane, infrastructure).\n" +
			"These are independent of --kubernetes-version; given together, the components\n" +
			"upgrade runs first. See docs/UPGRADES.md.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "kubernetes-version",
				Aliases: []string{"k"},
				Usage:   "target Kubernetes (node) version, e.g. v1.33.4; omit for a components-only upgrade",
			},
			&cli.StringFlag{
				Name:  "capi-version",
				Usage: "day-2: bump the Cluster API core + kubeadm bootstrap/control-plane providers to this version",
			},
			&cli.StringFlag{
				Name:  "provider-version",
				Usage: "day-2: bump the infrastructure provider to this version",
			},
			&cli.StringFlag{
				Name:  "operator-version",
				Usage: "day-2: bump the Cluster API operator itself (default: leave the operator and cert-manager alone)",
			},
			&cli.StringFlag{
				Name:  "capi-core-image",
				Usage: "day-2: fork/mirror image for the Cluster API core controller (with --capi-version)",
			},
			&cli.StringFlag{
				Name:  "capi-bootstrap-image",
				Usage: "day-2: fork/mirror image for the kubeadm bootstrap controller",
			},
			&cli.StringFlag{
				Name:  "capi-controlplane-image",
				Usage: "day-2: fork/mirror image for the kubeadm control-plane controller",
			},
			&cli.StringFlag{
				Name:  "provider-image",
				Usage: "day-2: fork/mirror image for the infrastructure provider controller",
			},
			&cli.BoolFlag{
				Name:  "autoscaler",
				Usage: "day-2: install/enable the Cluster Autoscaler add-on; --autoscaler=false removes it",
			},
			&cli.IntFlag{
				Name:  "autoscaler-min",
				Usage: "day-2: retune the autoscaler node-group minimum on every worker pool (patches the MachineDeployment annotation)",
			},
			&cli.IntFlag{
				Name:  "autoscaler-max",
				Usage: "day-2: retune the autoscaler node-group maximum on every worker pool",
			},
			&cli.StringFlag{
				Name:  "autoscaler-version",
				Usage: "day-2: bump the autoscaler image tag, e.g. v1.33.4",
			},
			&cli.StringFlag{
				Name:  "autoscaler-image",
				Usage: "day-2: fully override the autoscaler image (repo:tag), for a mirror or fork",
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
				Components: klotho.ComponentUpgrade{
					CAPIVersion:       c.String("capi-version"),
					ProviderVersion:   c.String("provider-version"),
					OperatorVersion:   c.String("operator-version"),
					CoreImage:         c.String("capi-core-image"),
					BootstrapImage:    c.String("capi-bootstrap-image"),
					ControlPlaneImage: c.String("capi-controlplane-image"),
					ProviderImage:     c.String("provider-image"),
				},
				Autoscaler: klotho.AutoscalerChange{
					Set:     c.IsSet("autoscaler"),
					Enabled: c.Bool("autoscaler"),
					Min:     int(c.Int("autoscaler-min")),
					MinSet:  c.IsSet("autoscaler-min"),
					Max:     int(c.Int("autoscaler-max")),
					MaxSet:  c.IsSet("autoscaler-max"),
					Version: c.String("autoscaler-version"),
					Image:   c.String("autoscaler-image"),
				},
			}
			// Validate before touching configuration or the cluster: the user must
			// ask for at least one of a node-version upgrade or a management-plane
			// component upgrade.
			if opts.KubernetesVersion == "" && !opts.Components.Requested() && !opts.Autoscaler.Requested() {
				return fmt.Errorf("upgrade: nothing to do: set --kubernetes-version and/or a component flag (--capi-version, --provider-version, --operator-version, an image flag) and/or an autoscaler flag (--autoscaler, --autoscaler-min/-max, --autoscaler-version)")
			}
			a, err := newApp(c)
			if err != nil {
				return err
			}
			if err := klotho.Upgrade(ctx, a.deps(), name, opts); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout(c), "upgraded %s: %s\n", name, upgradeSummary(opts))
			return nil
		},
	}
}

// upgradeSummary describes, in one line, what an upgrade changed, for the
// command's success message.
func upgradeSummary(opts klotho.UpgradeOptions) string {
	var parts []string
	if opts.KubernetesVersion != "" {
		parts = append(parts, "Kubernetes "+opts.KubernetesVersion)
	}
	c := opts.Components
	if c.CAPIVersion != "" {
		parts = append(parts, "CAPI "+c.CAPIVersion)
	}
	if c.ProviderVersion != "" {
		parts = append(parts, "provider "+c.ProviderVersion)
	}
	if c.OperatorVersion != "" {
		parts = append(parts, "operator "+c.OperatorVersion)
	}
	if a := opts.Autoscaler; a.Requested() {
		switch {
		case a.Set && !a.Enabled:
			parts = append(parts, "autoscaler removed")
		case a.Version != "" || a.Image != "":
			parts = append(parts, "autoscaler image")
		default:
			parts = append(parts, "autoscaler")
		}
	}
	if len(parts) == 0 {
		// Only image flags were given.
		return "component images"
	}
	return strings.Join(parts, ", ")
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
