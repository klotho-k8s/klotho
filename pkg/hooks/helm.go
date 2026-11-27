package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/yaml"
)

// helmActionFactory builds a Helm action configuration for a namespace against
// the kubeconfig at kubeconfigPath. defaultHelmActions is the real
// implementation; tests inject an in-memory configuration so install/upgrade
// runs without a cluster.
type helmActionFactory func(ctx context.Context, log *slog.Logger, hookName, kubeconfigPath, namespace string) (*action.Configuration, error)

// defaultHelmActions initializes Helm against the admin kubeconfig klotho wrote
// for the hook, using the Secret storage driver (Helm's default).
func defaultHelmActions(_ context.Context, log *slog.Logger, hookName, kubeconfigPath, namespace string) (*action.Configuration, error) {
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &kubeconfigPath
	flags.Namespace = &namespace

	cfg := new(action.Configuration)
	debug := func(format string, v ...any) {
		log.Info("helm", "name", hookName, "msg", fmt.Sprintf(format, v...))
	}
	if err := cfg.Init(flags, namespace, "secret", debug); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyHelm installs or upgrades the hook's Helm release against the booted
// cluster. The chart is located (a repository path, an oci:// reference, a
// packaged-chart URL, or a local path), values are merged (inline over file),
// and the release is installed when absent or upgraded when present. The
// namespace is created on install.
func (e Engine) applyHelm(ctx context.Context, h Hook, t Target) error {
	spec := h.Helm

	path, cleanup, err := writeKubeconfig(t.Kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	namespace := spec.Namespace
	if namespace == "" {
		namespace = "default"
	}

	factory := e.helmActions
	if factory == nil {
		factory = defaultHelmActions
	}
	cfg, err := factory(ctx, e.log, h.Name, path, namespace)
	if err != nil {
		return fmt.Errorf("helm %s: %w", spec.Chart, err)
	}

	settings := cli.New()
	vals, err := helmValues(spec)
	if err != nil {
		return err
	}

	cpo := action.ChartPathOptions{Version: spec.Version}
	chartPath, err := cpo.LocateChart(spec.Chart, settings)
	if err != nil {
		return fmt.Errorf("helm %s: locating chart: %w", spec.Chart, err)
	}
	ch, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("helm %s: loading chart: %w", spec.Chart, err)
	}

	release := spec.ReleaseName
	if release == "" {
		release = ch.Name()
	}

	hist := action.NewHistory(cfg)
	hist.Max = 1
	_, histErr := hist.Run(release)
	switch {
	case errors.Is(histErr, driver.ErrReleaseNotFound):
		e.log.Info("helm installing", "name", h.Name, "release", release, "chart", spec.Chart, "namespace", namespace)
		inst := action.NewInstall(cfg)
		inst.ReleaseName = release
		inst.Namespace = namespace
		inst.CreateNamespace = true
		inst.Version = spec.Version
		if _, err := inst.RunWithContext(ctx, ch, vals); err != nil {
			return fmt.Errorf("helm %s: install %q: %w", spec.Chart, release, err)
		}
		e.log.Info("helm installed", "name", h.Name, "release", release)
		return nil
	case histErr != nil:
		return fmt.Errorf("helm %s: history %q: %w", spec.Chart, release, histErr)
	default:
		e.log.Info("helm upgrading", "name", h.Name, "release", release, "chart", spec.Chart, "namespace", namespace)
		up := action.NewUpgrade(cfg)
		up.Namespace = namespace
		up.Version = spec.Version
		if _, err := up.RunWithContext(ctx, release, ch, vals); err != nil {
			return fmt.Errorf("helm %s: upgrade %q: %w", spec.Chart, release, err)
		}
		e.log.Info("helm upgraded", "name", h.Name, "release", release)
		return nil
	}
}

// helmValues merges the hook's values file (if any) with its inline values,
// inline taking precedence.
func helmValues(spec *HelmSpec) (map[string]any, error) {
	base := map[string]any{}
	if spec.ValuesFile != "" {
		b, err := os.ReadFile(spec.ValuesFile)
		if err != nil {
			return nil, fmt.Errorf("helm values %s: %w", spec.ValuesFile, err)
		}
		if err := yaml.Unmarshal(b, &base); err != nil {
			return nil, fmt.Errorf("helm values %s: %w", spec.ValuesFile, err)
		}
	}
	if len(spec.Values) > 0 {
		inline, err := deepCopyMap(spec.Values)
		if err != nil {
			return nil, fmt.Errorf("helm inline values: %w", err)
		}
		base = chartutil.CoalesceTables(inline, base)
	}
	return base, nil
}

// deepCopyMap copies a values map so merging does not mutate the hook spec.
func deepCopyMap(in map[string]any) (map[string]any, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
