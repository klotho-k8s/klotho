package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
				Usage:    "infrastructure provider (proxmox, docker, aws, linode, gcp, azure, hyperv, libvirt, vz)",
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
			&cli.BoolFlag{
				Name:    "yes",
				Aliases: []string{"y"},
				Usage:   "assume yes to prompts, e.g. installing the Hyper-V host agent",
			},
			&cli.BoolFlag{
				Name:  "no-install-agent",
				Usage: "never install the Hyper-V host agent; fail if it is not already present",
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
	if provider != providerProxmox && provider != providerDocker && provider != providerAWS && provider != providerLinode && provider != providerGCP && provider != providerAzure && provider != providerHyperv && provider != providerLibvirt && provider != providerVZ {
		return fmt.Errorf("create: infrastructure provider %q: %w (available: %s, %s, %s, %s, %s, %s, %s, %s, %s)", provider, ErrNotImplemented, providerProxmox, providerDocker, providerAWS, providerLinode, providerGCP, providerAzure, providerHyperv, providerLibvirt, providerVZ)
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
	if provider == providerHyperv {
		if err := a.ensureHyperVAgent(ctx, c); err != nil {
			return err
		}
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
	if err := a.embedLocalComponents(ctx, c, &req.CAPI); err != nil {
		return err
	}
	if req.Hooks, err = a.cfg.HookSpecs(); err != nil {
		return err
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
		_, _ = fmt.Fprintln(stdout(c), req.DryRunDir)
		return nil
	}
	_, _ = fmt.Fprintln(stdout(c), credential.ServerURL(st.Endpoint))
	return nil
}

// embedLocalComponents handles provider components served over a plain http URL,
// which the in-cluster Cluster API operator refuses to fetch ("no provider
// implementation for \"http\" schema") - the case for a local, unpublished
// provider build (such as the libvirt provider) or a local CAPI mirror. For each
// such URL it downloads the components (and the sibling metadata.yaml) and moves
// them onto o's embedded-components fields, so ProviderObjects serves them from a
// labeled ConfigMap via fetchConfig.selector instead of a URL. An https or oci
// URL is left untouched for the operator to fetch itself. It is
// provider-agnostic: it keys on the http scheme, not on any provider name.
//
// TODO(dedup): the Hyper-V provider branch carries an infrastructure-only copy
// of this (same name). De-duplicate once both branches merge.
func (a *app) embedLocalComponents(ctx context.Context, c *cli.Command, o *generate.CAPIOptions) error {
	// The infrastructure provider's components.
	if isLocalHTTP(o.InfraFetchURL) {
		comp, err := fetchComponents(ctx, o.InfraFetchURL)
		if err != nil {
			return fmt.Errorf("fetching %s for a local-provider ConfigMap: %w", o.InfraFetchURL, err)
		}
		o.InfraComponents = string(comp)
		if meta, err := fetchSibling(ctx, o.InfraFetchURL, "metadata.yaml"); err == nil {
			o.InfraMetadata = meta
		} else {
			_, _ = fmt.Fprintf(stderr(c), "%s: no metadata.yaml alongside components (%v); synthesizing one from the version\n", o.InfraProviderName, err)
		}
		_, _ = fmt.Fprintf(stderr(c), "%s: embedding the locally served components in a ConfigMap (%d bytes); the operator reads it via fetchConfig.selector\n", o.InfraProviderName, len(comp))
		o.InfraFetchURL = ""
	}

	// The core, bootstrap and control plane components (a CAPI mirror).
	if isLocalHTTP(o.FetchURL) {
		for _, f := range []struct {
			file string
			dst  *string
		}{
			{generate.CoreComponents, &o.CoreComponentsData},
			{generate.BootstrapComponents, &o.BootstrapComponentsData},
			{generate.ControlPlaneComponents, &o.ControlPlaneComponentsData},
		} {
			comp, err := fetchSibling(ctx, o.FetchURL, f.file)
			if err != nil {
				return fmt.Errorf("fetching %s for a local CAPI ConfigMap: %w", f.file, err)
			}
			*f.dst = comp
		}
		if meta, err := fetchSibling(ctx, o.FetchURL, "metadata.yaml"); err == nil {
			o.CAPIMetadata = meta
		} else {
			_, _ = fmt.Fprintf(stderr(c), "cluster-api: no metadata.yaml at %s (%v); synthesizing one from the version\n", o.FetchURL, err)
		}
		_, _ = fmt.Fprintf(stderr(c), "cluster-api: embedding the locally served core/bootstrap/control-plane components in ConfigMaps; the operator reads them via fetchConfig.selector\n")
		o.FetchURL = ""
	}
	return nil
}

// isLocalHTTP reports whether raw is a plain-http URL (the scheme the in-cluster
// operator rejects). An https, oci or file URL - or an empty one - returns false.
func isLocalHTTP(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http"
}

// fetchSibling GETs a file sitting next to base (base's last path segment
// replaced with name), for example the metadata.yaml alongside a components file.
func fetchSibling(ctx context.Context, base, name string) (string, error) {
	i := strings.LastIndex(base, "/")
	if i < 0 {
		return "", fmt.Errorf("no path in %q", base)
	}
	b, err := fetchComponents(ctx, base[:i+1]+name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// fetchComponents GETs url and returns its body, failing on a non-2xx status.
func fetchComponents(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	return io.ReadAll(resp.Body)
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
