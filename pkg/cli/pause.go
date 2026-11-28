package cli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
)

func newPauseCommand() *cli.Command {
	return &cli.Command{
		Name:      "pause",
		Usage:     "Pause Cluster API reconciliation of a cluster",
		ArgsUsage: "<name>",
		Description: "Sets spec.paused on the cluster's Cluster object, which stops every Cluster\n" +
			"API controller that owns the cluster from reconciling it: no scaling, rollout,\n" +
			"remediation or machine create/delete. It stops reconciliation only. The API\n" +
			"server, etcd, kubelets and workloads keep running, so pausing frees no\n" +
			"resources and lets no failed machine self-heal. Pausing also stalls an in-flight\n" +
			"deletion, so a cluster or machine being deleted is blocked unless --force.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "force",
				Usage: "pause even when a guard blocks it (an in-flight deletion); the guard is still reported, nothing is changed to resolve it",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			name, err := clusterNameArg(c)
			if err != nil {
				return err
			}
			a, err := newApp(c)
			if err != nil {
				return err
			}
			res, err := klotho.Pause(ctx, a.deps(), name, klotho.PauseOptions{Force: c.Bool("force")})
			if err != nil {
				return err
			}
			reportPause(c, res, "paused", "already paused")
			return nil
		},
	}
}

func newUnpauseCommand() *cli.Command {
	return &cli.Command{
		Name:      "unpause",
		Usage:     "Resume Cluster API reconciliation of a cluster",
		ArgsUsage: "<name>",
		Description: "Clears spec.paused on the cluster's Cluster object so the Cluster API\n" +
			"controllers resume reconciling it. Any lingering per-object pause annotations on\n" +
			"child objects are reported, because clearing spec.paused does not remove them.",
		Action: func(ctx context.Context, c *cli.Command) error {
			name, err := clusterNameArg(c)
			if err != nil {
				return err
			}
			a, err := newApp(c)
			if err != nil {
				return err
			}
			res, err := klotho.Unpause(ctx, a.deps(), name, klotho.PauseOptions{})
			if err != nil {
				return err
			}
			reportPause(c, res, "unpaused", "not paused")
			return nil
		},
	}
}

// reportPause prints the guard findings and a one-line result. changed is the
// word for an applied change ("paused"/"unpaused"); unchanged is for a no-op
// ("already paused"/"not paused").
func reportPause(c *cli.Command, res *klotho.PauseResult, changed, unchanged string) {
	w := stdout(c)
	for _, f := range res.Findings {
		_, _ = fmt.Fprintf(stderr(c), "  - %s\n", f)
	}
	if res.Changed {
		_, _ = fmt.Fprintf(w, "%s %s\n", changed, res.Cluster)
		return
	}
	_, _ = fmt.Fprintf(w, "%s is %s\n", res.Cluster, unchanged)
}
