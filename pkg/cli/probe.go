package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/urfave/cli/v3"

	proxmoxbackend "github.com/klotho-k8s/klotho/provider/backend/proxmox"
)

// newProbeCommand checks a Proxmox environment before any cluster is created:
// what the token can see, and whether a resource pool comment can hold a state
// record. It creates one temporary pool and deletes it. Hidden: it is a lab and
// support tool, not part of the everyday surface.
func newProbeCommand() *cli.Command {
	return &cli.Command{
		Name:   "probe",
		Usage:  "Check Proxmox access, inventory, and the pool-comment state store",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "skip-pool",
				Usage: "do not create a temporary pool to measure the comment size limit",
			},
			&cli.StringFlag{
				Name:  "pool-id",
				Usage: "temporary pool ID for the comment probe",
				Value: "klotho-probe",
			},
		},
		Action: runProbe,
	}
}

func runProbe(ctx context.Context, c *cli.Command) error {
	a, err := newApp(c)
	if err != nil {
		return err
	}
	out := stdout(c)
	infra, err := a.infra(providerProxmox)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "inventory:")
	inv, err := infra.Discover(ctx)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}
	kinds := make([]string, 0, len(inv))
	for k := range inv {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	for _, k := range kinds {
		items := inv[k]
		if len(items) == 0 {
			fmt.Fprintf(tw, "  %s\t(none visible)\t\n", k)
			continue
		}
		for _, it := range items {
			attrs := make([]string, 0, len(it.Attrs))
			for ak, av := range it.Attrs {
				attrs = append(attrs, ak+"="+av)
			}
			sort.Strings(attrs)
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", k, it.ID, it.Name, strings.Join(attrs, " "))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if c.Bool("skip-pool") {
		return nil
	}
	client, err := a.proxmoxClient()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\npool comment probe (temporary pool %q):\n", c.String("pool-id"))
	res, err := proxmoxbackend.Probe(ctx, client, c.String("pool-id"))
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	fmt.Fprintf(out, "  largest comment that round-trips: %d bytes\n", res.MaxSize)
	fmt.Fprintf(out, "  newlines survive: %v\n", res.NewlinesSurvive)
	for _, e := range res.Errors {
		fmt.Fprintf(out, "  note: %s\n", e)
	}
	return nil
}
