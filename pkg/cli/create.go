package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/config"
	"github.com/klotho-k8s/klotho/pkg/credential"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider/controlplane/kubeadm"
)

func newCreateCommand() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Usage:     "Create a self-managed cluster",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "provider",
				Aliases:  []string{"p"},
				Usage:    "infrastructure provider (proxmox, docker)",
				Required: true,
			},
			&cli.StringFlag{
				Name:    "control-plane",
				Aliases: []string{"c"},
				Usage:   "control plane provider (kubeadm)",
				Value:   cpKubeadm,
			},
			&cli.StringFlag{
				Name:  "flavor",
				Usage: "cluster template flavor",
			},
			&cli.StringFlag{
				Name:    "manifest",
				Aliases: []string{"m"},
				Usage:   "use this cluster manifest instead of generating one (- for stdin)",
			},
			&cli.StringFlag{
				Name:  "dry-run",
				Usage: "render manifests and user-data into this directory without provisioning",
			},
			&cli.StringFlag{
				Name:  "kubernetes-version",
				Usage: "Kubernetes version, e.g. v1.33.4 (KUBERNETES_VERSION)",
			},
			&cli.IntFlag{
				Name:  "control-plane-count",
				Usage: "control plane machines (CONTROL_PLANE_MACHINE_COUNT)",
				Value: 1,
			},
			&cli.IntFlag{
				Name:  "worker-count",
				Usage: "worker machines (WORKER_MACHINE_COUNT)",
				Value: 0,
			},
			&cli.StringSliceFlag{
				Name:  "var",
				Usage: "template variable `KEY=VALUE`; repeatable, wins over configuration",
			},
			&cli.StringFlag{
				Name:  "pool",
				Usage: "put every generated machine template in this infrastructure pool",
			},
			&cli.StringSliceFlag{
				Name:  "pod-cidr",
				Usage: "pod network `CIDR` for a generated bundle; repeatable for dual-stack",
			},
			&cli.StringSliceFlag{
				Name:  "service-cidr",
				Usage: "service network `CIDR` for a generated bundle; repeatable for dual-stack",
			},
			&cli.BoolFlag{
				Name:  "install-kubernetes",
				Usage: "install containerd and the bundle's Kubernetes version on every node at boot, for plain images (nodes.installKubernetes; --install-kubernetes=false overrides it)",
			},
			&cli.DurationFlag{
				Name:  "wait-timeout",
				Usage: "bound for each wait on the new cluster (0: 15m for the API server, 10m otherwise)",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "create even when a provider reports findings about the cluster manifest (they are still printed; the manifest is never changed)",
			},
		},
		Action: runCreate,
	}
}

func runCreate(ctx context.Context, c *cli.Command) error {
	name, err := clusterNameArg(c)
	if err != nil {
		return err
	}
	provider, cp := c.String("provider"), c.String("control-plane")
	if provider != providerProxmox && provider != providerDocker && provider != providerAWS {
		return fmt.Errorf("create: infrastructure provider %q: %w (available: %s, %s, %s)", provider, ErrNotImplemented, providerProxmox, providerDocker, providerAWS)
	}
	if cp != cpKubeadm {
		return fmt.Errorf("create: control plane provider %q: %w (available: %s)", cp, ErrNotImplemented, cpKubeadm)
	}
	vars, err := parseVars(c.StringSlice("var"))
	if err != nil {
		return err
	}
	var manifest []byte
	if m := c.String("manifest"); m != "" {
		if manifest, err = readManifest(c, m); err != nil {
			return err
		}
	}

	a, err := newApp(c)
	if err != nil {
		return err
	}
	kubeVersion := c.String("kubernetes-version")
	if kubeVersion == "" {
		kubeVersion = firstNonEmpty(vars[generate.VarKubernetesVersion], a.cfg.Variables[generate.VarKubernetesVersion])
	}
	deps, err := a.withProviders(provider, cp, kubeadm.Options{KubernetesVersion: kubeVersion})
	if err != nil {
		return err
	}
	pref := a.cfg.Providers[provider]
	req := klotho.CreateRequest{
		Spec: cluster.Spec{
			Name:      name,
			Flavor:    c.String("flavor"),
			Variables: vars,
		},
		Provider:             generate.ProviderRef{Name: provider, Version: pref.Version, URL: pref.FetchURL},
		Kubernetes:           kubeVersion,
		ControlPlaneReplicas: int(c.Int("control-plane-count")),
		WorkerReplicas:       int(c.Int("worker-count")),
		Manifest:             manifest,
		Pool:                 c.String("pool"),
		PodCIDRs:             c.StringSlice("pod-cidr"),
		ServiceCIDRs:         c.StringSlice("service-cidr"),
		DryRunDir:            c.String("dry-run"),
		Force:                c.Bool("force"),
		InstallKubernetes:    installKubernetes(c, a.cfg),
		InstallOptions:       a.cfg.Nodes.InstallOptions(),
		CAPI: generate.CAPIOptions{
			Version:              a.cfg.CAPI.Version,
			FetchURL:             a.cfg.CAPI.FetchURL,
			FeatureGates:         a.cfg.CAPI.FeatureGates,
			InfraProviderName:    provider,
			InfraProviderVersion: pref.Version,
			InfraFetchURL:        firstNonEmpty(pref.ComponentsURL, pref.FetchURL),
			InfraImage:           pref.Image,
			ProviderSecretData:   a.providerSecretData(provider),
		},
		DefaultVariables:  a.cfg.Variables,
		SSHAuthorizedKeys: a.cfg.SSHAuthorizedKeys,
	}
	if d := c.Duration("wait-timeout"); d > 0 {
		req.Timeouts = klotho.Timeouts{APIServer: d, Providers: d, Apply: d, Node: d}
	}
	if provider == providerProxmox && a.cfg.Proxmox.Template.Selects() {
		req.MachineTemplateTags = a.cfg.Proxmox.Template.Tags
		req.MachineTemplateMatchPolicy = a.cfg.Proxmox.Template.MatchPolicy
	}

	st, err := klotho.Create(ctx, deps, req)
	if err != nil {
		return err
	}
	if req.DryRunDir != "" {
		fmt.Fprintln(stdout(c), req.DryRunDir)
		return nil
	}
	fmt.Fprintln(stdout(c), credential.ServerURL(st.Endpoint))
	return nil
}

// installKubernetes decides whether the install-at-boot script goes into the
// generated bundle: the --install-kubernetes flag when given (either way),
// else nodes.installKubernetes from the configuration.
func installKubernetes(c *cli.Command, cfg *config.Config) bool {
	if c.IsSet("install-kubernetes") {
		return c.Bool("install-kubernetes")
	}
	return cfg.Nodes.InstallKubernetes
}

// parseVars parses repeated KEY=VALUE flags.
func parseVars(in []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range in {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("--var %q: want KEY=VALUE", kv)
		}
		out[k] = v
	}
	return out, nil
}

func readManifest(c *cli.Command, path string) ([]byte, error) {
	if path == "-" {
		b, err := io.ReadAll(stdin(c))
		if err != nil {
			return nil, fmt.Errorf("--manifest: reading stdin: %w", err)
		}
		return b, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--manifest: %w", err)
	}
	return b, nil
}

// age renders a duration the way kubectl does: the largest unit or two.
func age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
