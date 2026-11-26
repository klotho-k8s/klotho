package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/provider/controlplane/kubeadm"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func newDeleteCommand() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "Delete a cluster and everything it owns",
		ArgsUsage: "<name>",
		Description: "By default klotho issues the Cluster API deletion and returns, leaving a\n" +
			"detached sidecar to finish the teardown (destroy the last node, free the\n" +
			"bootstrap ISO, clean up after itself). Use --watch to sweep in-process\n" +
			"instead, or --force when the cluster's API is already gone.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "yes",
				Aliases: []string{"y"},
				Usage:   "skip the confirmation prompt",
			},
			&cli.BoolFlag{
				Name:  "watch",
				Usage: "follow the Cluster API deletion to completion and sweep in-process, instead of returning and leaving a sidecar to finish",
			},
			&cli.BoolFlag{
				Name:  "graceful",
				Usage: "alias for --watch",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "skip Cluster API and remove the infrastructure directly by tag; use when the cluster's API is already gone",
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
			st, err := klotho.Load(ctx, a.deps(), name)
			if err != nil {
				return err
			}
			deps := a.deps()
			if !st.Reserved {
				provider := st.Spec.Provider
				if provider == "" {
					provider = providerProxmox
				}
				if deps, err = a.withProviders(provider, cpKubeadm, kubeadm.Options{}); err != nil {
					return err
				}
			}
			opts := klotho.DeleteOptions{
				Watch:    c.Bool("watch"),
				Graceful: c.Bool("graceful"),
				Force:    c.Bool("force"),
			}
			if !c.Bool("yes") {
				opts.Confirm = func(p *infrastructure.Plan) bool { return confirm(c, name, p) }
			}
			err = klotho.Delete(ctx, deps, name, opts)
			if errors.Is(err, klotho.ErrDeclined) {
				fmt.Fprintln(stderr(c), "aborted")
				return nil
			}
			if err != nil {
				return err
			}
			_ = os.Remove(filepath.Join(a.cfg.CredentialsDir(), name+".json"))
			fmt.Fprintf(stdout(c), "deleted %s\n", name)
			return nil
		},
	}
}

// confirm shows the plan and reads a yes/no answer.
func confirm(c *cli.Command, name string, p *infrastructure.Plan) bool {
	w := stderr(c)
	fmt.Fprintf(w, "Deleting cluster %s removes:\n", name)
	if len(p.Resources) == 0 {
		fmt.Fprintln(w, "  (no infrastructure found; only the state record)")
	}
	for _, r := range p.Resources {
		line := "  " + r.Kind + " " + r.ID
		if r.Name != "" {
			line += " (" + r.Name + ")"
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprint(w, "Proceed? [y/N] ")
	answer, _ := bufio.NewReader(stdin(c)).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
