package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/urfave/cli/v3"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// redacted replaces secret values in printed state.
const redacted = "REDACTED"

func newStateCommand() *cli.Command {
	return &cli.Command{
		Name:      "state",
		Usage:     "Print the persisted state of a cluster",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "output format (yaml, json)",
				Value:   "yaml",
			},
			&cli.BoolFlag{
				Name:  "show-secrets",
				Usage: "include the cluster CA private key",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			name, err := clusterNameArg(c)
			if err != nil {
				return err
			}
			format := c.String("output")
			if format != "yaml" && format != "json" {
				return fmt.Errorf("state: unknown output format %q (yaml, json)", format)
			}
			a, err := newApp(c)
			if err != nil {
				return err
			}
			st, err := klotho.Load(ctx, a.deps(), name)
			if err != nil {
				return err
			}
			view, err := stateView(st, c.Bool("show-secrets"))
			if err != nil {
				return err
			}
			var out []byte
			if format == "json" {
				out, err = json.MarshalIndent(view, "", "  ")
				out = append(out, '\n')
			} else {
				out, err = yaml.Marshal(view)
			}
			if err != nil {
				return err
			}
			_, err = stdout(c).Write(out)
			return err
		},
	}
}

// stateView renders state for people: PEM as text rather than base64, and
// the CA key redacted unless showSecrets.
func stateView(st *cluster.State, showSecrets bool) (map[string]any, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	ca := map[string]any{}
	if len(st.CA.Cert) > 0 {
		ca["cert"] = string(st.CA.Cert)
	}
	if len(st.CA.Key) > 0 {
		if showSecrets {
			ca["key"] = string(st.CA.Key)
		} else {
			ca["key"] = redacted
		}
	}
	m["ca"] = ca
	return m, nil
}
