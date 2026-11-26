package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/credential"
)

// nodeProbeTimeout bounds the live node query per cluster.
const nodeProbeTimeout = 10 * time.Second

func newListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List clusters known to the backend",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "nodes",
				Usage: "query each cluster for its live node status",
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "output format (table, json)",
				Value:   "table",
			},
		},
		Action: runList,
	}
}

// listRow is one cluster in list output.
type listRow struct {
	klotho.Summary
	Nodes *nodeStatus `json:"nodes,omitempty"`
}

type nodeStatus struct {
	Total int    `json:"total"`
	Ready int    `json:"ready"`
	Error string `json:"error,omitempty"`
}

func runList(ctx context.Context, c *cli.Command) error {
	format := c.String("output")
	if format != "table" && format != "json" {
		return fmt.Errorf("list: unknown output format %q (table, json)", format)
	}
	a, err := newApp(c)
	if err != nil {
		return err
	}
	deps := a.deps()
	sums, err := klotho.List(ctx, deps)
	if err != nil {
		return err
	}
	rows := make([]listRow, len(sums))
	for i, s := range sums {
		rows[i] = listRow{Summary: s}
	}
	if c.Bool("nodes") {
		var wg sync.WaitGroup
		for i := range rows {
			if rows[i].Reserved || rows[i].Error != "" {
				continue
			}
			wg.Add(1)
			go func(r *listRow) {
				defer wg.Done()
				ns := &nodeStatus{}
				st, err := klotho.Load(ctx, deps, r.Name)
				if err != nil {
					ns.Error = err.Error()
				} else {
					*ns = probeNodes(ctx, st)
				}
				r.Nodes = ns
			}(&rows[i])
		}
		wg.Wait()
	}

	w := stdout(c)
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	header := "NAME\tPROVIDER\tCONTROL-PLANE\tENDPOINT\tAGE\tRESERVED"
	if c.Bool("nodes") {
		header += "\tNODES\tREADY"
	}
	_, _ = fmt.Fprintln(tw, header)
	now := time.Now()
	for _, r := range rows {
		endpoint, ageStr := "-", "-"
		if r.Endpoint.Host != "" {
			endpoint = fmt.Sprintf("%s:%d", r.Endpoint.Host, r.Endpoint.Port)
		}
		if !r.CreatedAt.IsZero() {
			ageStr = age(now.Sub(r.CreatedAt))
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%v", r.Name, dash(r.Provider), dash(r.ControlPlane), endpoint, ageStr, r.Reserved)
		if r.Error != "" {
			line = fmt.Sprintf("%s\t<unreadable: %s>\t\t\t\t", r.Name, r.Error)
		}
		if c.Bool("nodes") {
			switch {
			case r.Nodes == nil:
				line += "\t-\t-"
			case r.Nodes.Error != "":
				line += "\tunreachable\t-"
			default:
				line += fmt.Sprintf("\t%d\t%d", r.Nodes.Total, r.Nodes.Ready)
			}
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// probeNodes lists a cluster's nodes with a freshly minted credential.
func probeNodes(ctx context.Context, st *cluster.State) nodeStatus {
	cs, err := clientFor(st, nodeProbeTimeout)
	if err != nil {
		return nodeStatus{Error: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, nodeProbeTimeout)
	defer cancel()
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nodeStatus{Error: err.Error()}
	}
	ns := nodeStatus{Total: len(nodes.Items)}
	for _, n := range nodes.Items {
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ns.Ready++
			}
		}
	}
	return ns
}

// clientFor returns a clientset authenticated with a short-lived
// certificate minted from the state's CA.
func clientFor(st *cluster.State, timeout time.Duration) (*kubernetes.Clientset, error) {
	cert, key, err := credential.Mint(st.CA, st.Spec.Name, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(&rest.Config{
		Host:            credential.ServerURL(st.Endpoint),
		Timeout:         timeout,
		UserAgent:       "klotho",
		TLSClientConfig: rest.TLSClientConfig{CAData: st.CA.Cert, CertData: cert, KeyData: key},
	})
}
