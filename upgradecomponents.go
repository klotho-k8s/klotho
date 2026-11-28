package klotho

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/provider"
)

// ComponentUpgrade is the day-2 upgrade of the Cluster API management-plane
// components - the controllers themselves - requested alongside (or instead of)
// a Kubernetes version bump. It is independent of UpgradeOptions.KubernetesVersion.
//
// klotho installs Cluster API through the Cluster API operator, whose
// CoreProvider / BootstrapProvider / ControlPlaneProvider / InfrastructureProvider
// CRs are declarative version objects: spec.version selects the component
// manifests, CRDs and contract, and the manager container's imageUrl overrides
// the controller image. A component upgrade is therefore a patch of those CRs:
// klotho reads the running CRs, re-applies them at the target version and image
// (reusing the create-time image lever in pkg/generate, applied day-2), and the
// operator re-fetches manifests, rolls CRDs and conversion webhooks, and rolls
// the controller deployment.
type ComponentUpgrade struct {
	// CAPIVersion bumps the three Cluster API-org providers together: the core
	// provider and, when they are the kubeadm providers, the bootstrap and
	// control-plane providers (the CAPI org releases them in lockstep). A cluster
	// whose control plane is k0s/rke2/k3s carries its own bootstrap/control-plane
	// providers from another project at their own versions; CAPIVersion does not
	// touch them (handled gracefully - see upgradeComponents).
	CAPIVersion string
	// ProviderVersion bumps the infrastructure provider.
	ProviderVersion string
	// CoreImage, BootstrapImage, ControlPlaneImage and ProviderImage override
	// the manager image of the matching provider, for fork or mirror images.
	// They are the same levers create uses, supplied day-2. Empty leaves the
	// running provider's image untouched.
	CoreImage         string
	BootstrapImage    string
	ControlPlaneImage string
	ProviderImage     string
	// OperatorVersion, when set, bumps the Cluster API operator itself. Empty
	// leaves the operator (and cert-manager) alone, which is the default: the
	// operator is only bumped when the target components need a newer one.
	OperatorVersion string
}

// Requested reports whether any management-plane component upgrade was asked
// for.
func (c ComponentUpgrade) Requested() bool {
	return c.CAPIVersion != "" || c.ProviderVersion != "" || c.OperatorVersion != "" ||
		c.CoreImage != "" || c.BootstrapImage != "" || c.ControlPlaneImage != "" || c.ProviderImage != ""
}

// liveProvider is a running operator provider CR, with the version and manager
// image read back from it.
type liveProvider struct {
	kind    string
	obj     *unstructured.Unstructured
	version string
	image   string // "" means the operator uses its built-in default image
}

func (p liveProvider) ref() string {
	return p.kind + " " + p.obj.GetNamespace() + "/" + p.obj.GetName()
}

// liveProviders are the four running operator provider CRs discovered in the
// cluster. Any may be nil if the cluster does not run it (rare).
type liveProviders struct {
	core         *liveProvider
	bootstrap    *liveProvider
	controlPlane *liveProvider
	infra        *liveProvider
}

// discoverProviders reads the running Cluster API operator provider CRs: the
// core, bootstrap, control-plane and infrastructure providers, across all
// namespaces. It reads each provider's current spec.version and manager image so
// the upgrade knows what it is moving from and whether the image is a fork pin.
func discoverProviders(ctx context.Context, cl kube.Cluster) (*liveProviders, error) {
	lp := &liveProviders{}
	for kind, dst := range map[string]**liveProvider{
		generate.KindCoreProvider:           &lp.core,
		generate.KindBootstrapProvider:      &lp.bootstrap,
		generate.KindControlPlaneProvider:   &lp.controlPlane,
		generate.KindInfrastructureProvider: &lp.infra,
	} {
		objs, err := cl.List(ctx, generate.OperatorGVK(kind), "")
		if err != nil {
			// The operator CRDs should be present on a klotho cluster; a List
			// error is more likely a transient API issue than an absent kind, so
			// surface it rather than silently treating the provider as missing.
			return nil, fmt.Errorf("listing %s: %w", kind, err)
		}
		if len(objs) == 0 {
			continue
		}
		// klotho installs exactly one of each; if more than one exists take the
		// first deterministically and note it.
		obj := objs[0]
		version, _, _ := unstructured.NestedString(obj.Object, "spec", "version")
		*dst = &liveProvider{kind: kind, obj: obj, version: version, image: generate.ManagerImage(obj)}
	}
	if lp.core == nil {
		return nil, fmt.Errorf("no %s found; is this a klotho-managed Cluster API cluster?", generate.KindCoreProvider)
	}
	return lp, nil
}

// componentPlan is the resolved target for one provider: its new version and
// image, and whether the version actually changes.
type componentPlan struct {
	live        *liveProvider
	version     string // target version (may equal live.version)
	image       string // target image override ("" leaves live image as is)
	clearImage  bool   // remove the image override (revert to operator default)
	versionBump bool   // the version changes
}

// plan resolves the four providers against the requested upgrade. A provider
// with no change gets a nil plan. Bootstrap and control-plane take the CAPI-org
// version only when they are the kubeadm providers; a non-kubeadm (k0s/rke2/k3s)
// provider is left alone by --capi-version.
func (c ComponentUpgrade) plan(lp *liveProviders) []componentPlan {
	var plans []componentPlan
	add := func(live *liveProvider, version, image string, kubeadmOnly bool) {
		if live == nil {
			return
		}
		if kubeadmOnly && version != "" && !isKubeadmProvider(live.obj.GetName()) {
			version = "" // --capi-version does not apply to a non-kubeadm provider
		}
		p := componentPlan{live: live, version: live.version, image: ""}
		if version != "" && version != live.version {
			p.version = version
			p.versionBump = true
		}
		if image != "" {
			p.image = image
		}
		if p.versionBump || p.image != "" {
			plans = append(plans, p)
		}
	}
	add(lp.core, c.CAPIVersion, c.CoreImage, false)
	add(lp.bootstrap, c.CAPIVersion, c.BootstrapImage, true)
	add(lp.controlPlane, c.CAPIVersion, c.ControlPlaneImage, true)
	add(lp.infra, c.ProviderVersion, c.ProviderImage, false)
	return plans
}

// isKubeadmProvider reports whether an operator provider name is the CAPI-org
// kubeadm provider, which the CAPI org releases in lockstep with core. A
// control plane built on k0s/rke2/k3s names its providers otherwise.
func isKubeadmProvider(name string) bool { return name == generate.KubeadmProviderName }

// componentFindings runs the management-plane upgrade guards. It returns the
// blocking findings (which stop the upgrade unless Force) and the warnings
// (always reported, never blocking). klotho reports and blocks; it never edits
// the cluster to resolve a finding.
func componentFindings(lp *liveProviders, cu ComponentUpgrade, plans []componentPlan) (blocking, warnings []provider.Finding) {
	// 1. Fork-image pinning (critical): a version bump of a provider whose
	//    running controller carries an explicit image override (a fork or mirror
	//    pin, such as klotho's self-hosted-delete cluster-api or hardened CAPMOX)
	//    without a matching --*-image would roll the deployment to an image still
	//    tagged for the old version - or, if the override were dropped, silently
	//    swap in the upstream default and lose the fork behavior. Refuse by
	//    default.
	for _, p := range plans {
		if p.versionBump && p.live.image != "" && p.image == "" {
			blocking = append(blocking, provider.Finding{
				Object: p.live.ref(),
				Field:  "spec.deployment.containers[manager].imageUrl",
				Message: fmt.Sprintf("the running controller image %q is a fork/mirror override, and a bump to %s with no matching image flag would roll the controller to an image tagged for the old version or revert it to the upstream default, losing the fork's behavior; supply the matching rebuilt image (for example --capi-core-image / --provider-image) or --force to proceed as is",
					p.live.image, p.version),
			})
		}
	}

	// 2. Contract compatibility (most important): the Cluster API contract the
	//    target core serves must be implemented by every provider. When the core
	//    crosses a contract boundary, a dependent provider that is not bumped
	//    alongside it may still implement the old contract and fail to reconcile.
	if cu.CAPIVersion != "" && lp.core != nil {
		fromC := capiCoreContract(lp.core.version)
		toC := capiCoreContract(cu.CAPIVersion)
		if fromC != "" && toC != "" && fromC != toC {
			for _, dep := range []*liveProvider{lp.bootstrap, lp.controlPlane, lp.infra} {
				if dep == nil {
					continue
				}
				if planFor(plans, dep) == nil {
					blocking = append(blocking, provider.Finding{
						Object: dep.ref(),
						Field:  "spec.version",
						Message: fmt.Sprintf("core moves from the %s to the %s Cluster API contract, but %s stays at %s; a provider built for %s will not reconcile against the new core - bump it to a %s-implementing release or confirm its metadata.yaml already declares %s",
							fromC, toC, dep.obj.GetName(), dep.version, fromC, toC, toC),
					})
				}
			}
		}
	}

	// 3. cert-manager floor: the Cluster API operator depends on cert-manager
	//    for its conversion/validating webhooks. Report the floor the target
	//    needs; klotho leaves cert-manager alone by default.
	if cu.CAPIVersion != "" || cu.OperatorVersion != "" {
		target := cu.OperatorVersion
		if target == "" {
			target = cu.CAPIVersion
		}
		floor := certManagerFloor(target)
		warnings = append(warnings, provider.Finding{
			Object:  "cert-manager",
			Field:   "version",
			Message: fmt.Sprintf("the target Cluster API needs cert-manager %s or newer for its webhooks; klotho does not touch cert-manager - verify it is at or above the floor before upgrading (set --operator-version only if the operator itself must move)", floor),
		})
	}

	// 4. Skew: the operator/clusterctl upgrade one minor at a time. Flag a jump
	//    across more than one minor, and a downgrade, on any bumped provider.
	for _, p := range plans {
		if !p.versionBump {
			continue
		}
		from, errFrom := parseUpgradeVersion(p.live.version)
		to, errTo := parseUpgradeVersion(p.version)
		if errFrom != nil || errTo != nil {
			continue // an unparseable version cannot be skew-checked; do not block
		}
		if to.LT(from) {
			blocking = append(blocking, provider.Finding{
				Object:  p.live.ref(),
				Field:   "spec.version",
				Message: fmt.Sprintf("target %s is a downgrade from the running %s; Cluster API components are not downgraded in place", p.version, p.live.version),
			})
		} else if jump := minorJump(from, to); jump > 1 {
			blocking = append(blocking, provider.Finding{
				Object:  p.live.ref(),
				Field:   "spec.version",
				Message: fmt.Sprintf("target %s is %d minor versions above the running %s; upgrade one minor at a time so CRDs and conversion webhooks step cleanly", p.version, jump, p.live.version),
			})
		}
	}

	// 5. Self-managed caveat: here management == workload, so the controllers
	//    being upgraded are the ones doing the upgrade. A bad component upgrade
	//    can strand reconciliation with no external management cluster to repair
	//    from. Warn and give the backup command.
	warnings = append(warnings, provider.Finding{
		Object: "self-managed cluster",
		Field:  "backup",
		Message: "this is a self-managed cluster: the Cluster API controllers being upgraded also drive the upgrade, so a bad upgrade can strand reconciliation with no external management cluster to repair from. Back the objects up first: " +
			"kubectl get coreprovider,bootstrapprovider,controlplaneprovider,infrastructureprovider,clusters,machines,machinedeployments,kubeadmcontrolplanes -A -o yaml > capi-backup.yaml",
	})

	return blocking, warnings
}

// planFor returns the plan for a given live provider, or nil when it is not
// being changed.
func planFor(plans []componentPlan, live *liveProvider) *componentPlan {
	for i := range plans {
		if plans[i].live == live {
			return &plans[i]
		}
	}
	return nil
}

// capiCoreContract returns the Cluster API contract the given CAPI core version
// serves. The v1beta2 contract is the current one klotho targets (core v1.11+);
// earlier releases serve v1beta1. An unparseable version returns "".
func capiCoreContract(version string) string {
	v, err := parseUpgradeVersion(version)
	if err != nil {
		return ""
	}
	switch {
	case v.Major > 1:
		return "v1beta2"
	case v.Major == 1 && v.Minor >= 11:
		return "v1beta2"
	case v.Major == 1 && v.Minor >= 2:
		return "v1beta1"
	default:
		return "v1alpha4"
	}
}

// certManagerFloor returns the minimum cert-manager version the given Cluster
// API target needs. The floor tracks Cluster API's documented requirement; it
// is advice, not a value klotho enforces.
func certManagerFloor(capiVersion string) string {
	v, err := parseUpgradeVersion(capiVersion)
	if err != nil {
		return "v1.15.0"
	}
	if v.Major == 1 && v.Minor >= 11 {
		return "v1.16.0"
	}
	return "v1.15.0"
}

// targetProviderCR builds the operator provider CR to apply for a plan: it
// starts from the running object (preserving fetchConfig, configSecret,
// tolerations, affinity, feature gates and anything else the operator or klotho
// set), drops status, sets spec.version to the target, and applies the image
// lever. Copying the live object is deliberate for a self-managed day-2 op:
// nothing the running controller depends on (its credential Secret, its node
// placement) is dropped, and only the version and image move.
func targetProviderCR(p componentPlan) *unstructured.Unstructured {
	u := p.live.obj.DeepCopy()
	u.SetResourceVersion("")
	u.SetUID("")
	u.SetManagedFields(nil)
	unstructured.RemoveNestedField(u.Object, "status")
	_ = unstructured.SetNestedField(u.Object, p.version, "spec", "version")
	if p.clearImage {
		unstructured.RemoveNestedField(u.Object, "spec", "deployment", "containers")
	} else if p.image != "" {
		// Reuse the create-time lever, applied day-2.
		generate.SetManagerImage(u, p.image)
	}
	return u
}

// upgradeComponents performs the management-plane upgrade: it discovers the
// running operator provider CRs, runs the guards, then re-applies each changed
// provider CR at its target version/image and waits for it to report Ready, in
// the safe order core -> bootstrap -> control-plane -> infrastructure. The
// operator re-fetches the components, rolls the CRDs and conversion webhooks,
// and rolls the controller deployment for each.
func upgradeComponents(ctx context.Context, deps Deps, cl kube.Cluster, cu ComponentUpgrade, force bool, log *slog.Logger) error {
	lp, err := discoverProviders(ctx, cl)
	if err != nil {
		return err
	}
	logDiscovered(log, lp)

	plans := cu.plan(lp)
	if len(plans) == 0 {
		log.Info("every requested component is already at the target version and image; nothing to apply")
		return nil
	}

	// Note the non-kubeadm bootstrap/control-plane providers --capi-version does
	// not touch, so the user is not surprised they stayed put.
	if cu.CAPIVersion != "" {
		for _, dep := range []*liveProvider{lp.bootstrap, lp.controlPlane} {
			if dep != nil && !isKubeadmProvider(dep.obj.GetName()) {
				log.Warn("--capi-version does not apply to a non-kubeadm provider; bump it through its own project's release",
					"provider", dep.obj.GetName(), "kind", dep.kind, "version", dep.version)
			}
		}
	}

	blocking, warnings := componentFindings(lp, cu, plans)
	for _, w := range warnings {
		log.Warn("upgrade advisory", "object", w.Object, "field", w.Field, "detail", w.Message)
	}
	for _, f := range blocking {
		log.Warn("component finding", "object", f.Object, "field", f.Field, "detail", f.Message)
	}
	if len(blocking) > 0 {
		if !force {
			return &UpgradeFindingsError{Findings: blocking}
		}
		log.Warn("proceeding despite component findings", "findings", len(blocking), "force", true)
	}

	// Apply and wait in the safe order: core first (it owns the shared CRDs and
	// the contract), then bootstrap and control-plane, then infrastructure.
	ordered := orderPlans(plans, lp)
	for _, p := range ordered {
		u := targetProviderCR(p)
		action := "image " + p.image
		if p.versionBump {
			action = p.live.version + " -> " + p.version
			if p.image != "" {
				action += " (image " + p.image + ")"
			}
		}
		log.Info("applying management-plane component", "provider", p.live.ref(), "change", action)
		if err := cl.Apply(ctx, kube.FieldManager, u); err != nil {
			return fmt.Errorf("applying %s at %s: %w", p.live.ref(), p.version, err)
		}
		ns, name, gvk := p.live.obj.GetNamespace(), p.live.obj.GetName(), p.live.obj.GroupVersionKind()
		if err := waitFor(ctx, deps, DefaultWatchTimeout, p.live.ref()+" to roll out", func(ctx context.Context) (bool, string) {
			cur, err := cl.Get(ctx, gvk, ns, name)
			if err != nil {
				return false, err.Error()
			}
			if v, _, _ := unstructured.NestedString(cur.Object, "spec", "version"); v != p.version {
				return false, "spec.version still " + v
			}
			return generate.ProviderReady(cur)
		}); err != nil {
			return err
		}
		log.Info("component ready", "provider", p.live.ref(), "version", p.version)
	}
	log.Info("management-plane components upgraded", "providers", len(ordered))
	return nil
}

// orderPlans returns the plans in the safe apply order: core, bootstrap,
// control-plane, infrastructure.
func orderPlans(plans []componentPlan, lp *liveProviders) []componentPlan {
	order := []*liveProvider{lp.core, lp.bootstrap, lp.controlPlane, lp.infra}
	var out []componentPlan
	for _, live := range order {
		if live == nil {
			continue
		}
		if p := planFor(plans, live); p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func logDiscovered(log *slog.Logger, lp *liveProviders) {
	for _, p := range []*liveProvider{lp.core, lp.bootstrap, lp.controlPlane, lp.infra} {
		if p == nil {
			continue
		}
		img := p.image
		if img == "" {
			img = "(operator default)"
		}
		log.Info("discovered management-plane component", "provider", p.ref(), "version", p.version, "image", img)
	}
}
