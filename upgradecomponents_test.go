package klotho

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider"
)

// --- builders ---------------------------------------------------------------

// providerCR builds a running operator provider CR the way create's
// ProviderObjects would: spec.version, a deployment block (tolerations/affinity
// stand in here as an empty map), an optional manager image override, and - for
// the infrastructure provider - a configSecret the controller depends on.
func providerCR(kind, ns, name, version, image string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(generate.OperatorGVK(kind))
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, version, "spec", "version")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{"replicas": int64(1)}, "spec", "deployment")
	if image != "" {
		generate.SetManagerImage(u, image)
	}
	return u
}

func liveOf(kind, ns, name, version, image string) *liveProvider {
	obj := providerCR(kind, ns, name, version, image)
	return &liveProvider{kind: kind, obj: obj, version: version, image: image}
}

// standardProviders is the common running topology: a klotho-installed kubeadm
// cluster on some infrastructure provider.
func standardProviders(coreVer, infraVer, coreImg, infraImg string) *liveProviders {
	return &liveProviders{
		core:         liveOf(generate.KindCoreProvider, generate.CoreNamespace, generate.CoreProviderName, coreVer, coreImg),
		bootstrap:    liveOf(generate.KindBootstrapProvider, generate.BootstrapNamespace, generate.KubeadmProviderName, coreVer, ""),
		controlPlane: liveOf(generate.KindControlPlaneProvider, generate.ControlPlaneNamespace, generate.KubeadmProviderName, coreVer, ""),
		infra:        liveOf(generate.KindInfrastructureProvider, "capmox-system", "proxmox", infraVer, infraImg),
	}
}

// --- plan -------------------------------------------------------------------

func TestComponentPlan(t *testing.T) {
	lp := standardProviders("v1.12.9", "v0.9.2", "", "")
	cu := ComponentUpgrade{CAPIVersion: "v1.13.0", ProviderVersion: "v0.10.0"}
	plans := cu.plan(lp)
	// core, bootstrap, control-plane and infra all move.
	if len(plans) != 4 {
		t.Fatalf("got %d plans, want 4: %+v", len(plans), plans)
	}
	for _, p := range plans {
		if !p.versionBump {
			t.Errorf("%s: expected a version bump", p.live.ref())
		}
	}
}

func TestComponentPlanNonKubeadmSkipped(t *testing.T) {
	lp := standardProviders("v1.12.9", "v0.9.2", "", "")
	// A k0s cluster: the bootstrap/control-plane providers are another project.
	lp.bootstrap = liveOf(generate.KindBootstrapProvider, "k0smotron", "k0sproject-k0smotron", "v1.3.0", "")
	lp.controlPlane = liveOf(generate.KindControlPlaneProvider, "k0smotron", "k0sproject-k0smotron", "v1.3.0", "")
	cu := ComponentUpgrade{CAPIVersion: "v1.13.0"}
	plans := cu.plan(lp)
	// Only core moves; --capi-version does not touch the non-kubeadm providers.
	if len(plans) != 1 || plans[0].live.kind != generate.KindCoreProvider {
		t.Fatalf("got %+v, want only the core provider bumped", plans)
	}
}

func TestComponentPlanImageOnly(t *testing.T) {
	lp := standardProviders("v1.12.9", "v0.9.2", "", "")
	cu := ComponentUpgrade{ProviderImage: "ghcr.io/me/capmox:v0.9.2-fix"}
	plans := cu.plan(lp)
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1 (infra image only): %+v", len(plans), plans)
	}
	if plans[0].versionBump || plans[0].image != "ghcr.io/me/capmox:v0.9.2-fix" {
		t.Fatalf("infra plan = %+v", plans[0])
	}
}

func TestComponentPlanNoOp(t *testing.T) {
	lp := standardProviders("v1.12.9", "v0.9.2", "", "")
	// Requesting the versions already running changes nothing.
	cu := ComponentUpgrade{CAPIVersion: "v1.12.9", ProviderVersion: "v0.9.2"}
	if plans := cu.plan(lp); len(plans) != 0 {
		t.Fatalf("want no plans when already at target, got %+v", plans)
	}
}

// --- guards -----------------------------------------------------------------

func TestForkImageGuard(t *testing.T) {
	cases := []struct {
		name      string
		coreImg   string
		coreImage string // the --capi-core-image supplied
		wantBlock bool
	}{
		{name: "fork image, no flag, blocked", coreImg: "ghcr.io/klotho-k8s/cluster-api-controller:v1.12.9", wantBlock: true},
		{name: "fork image, flag supplied, allowed", coreImg: "ghcr.io/klotho-k8s/cluster-api-controller:v1.12.9", coreImage: "ghcr.io/klotho-k8s/cluster-api-controller:v1.13.0"},
		{name: "upstream default, no flag, allowed", coreImg: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lp := standardProviders("v1.12.9", "v0.9.2", tc.coreImg, "")
			cu := ComponentUpgrade{CAPIVersion: "v1.13.0", CoreImage: tc.coreImage}
			blocking, _ := componentFindings(lp, cu, cu.plan(lp))
			got := hasFinding(blocking, "fork/mirror override")
			if got != tc.wantBlock {
				t.Fatalf("fork-image blocking = %v, want %v: %v", got, tc.wantBlock, blocking)
			}
		})
	}
}

func TestContractGuard(t *testing.T) {
	// Core crosses the v1beta1 -> v1beta2 boundary (v1.10 -> v1.12) while the
	// infra provider is left at its old version: a blocking contract finding.
	lp := standardProviders("v1.10.0", "v0.9.2", "", "")
	cu := ComponentUpgrade{CAPIVersion: "v1.12.9"} // provider not bumped
	blocking, _ := componentFindings(lp, cu, cu.plan(lp))
	if !hasFinding(blocking, "contract") {
		t.Fatalf("want a contract finding for the un-bumped infra provider: %v", blocking)
	}

	// Bump the infra provider alongside core: the contract finding clears for it.
	cu2 := ComponentUpgrade{CAPIVersion: "v1.12.9", ProviderVersion: "v0.11.0"}
	blocking2, _ := componentFindings(lp, cu2, cu2.plan(lp))
	for _, f := range blocking2 {
		if f.Object == lp.infra.ref() && contains(f.Message, "contract") {
			t.Fatalf("infra contract finding should clear when the provider is bumped: %v", f)
		}
	}

	// A within-contract bump (v1.12 -> v1.13, both v1beta2) raises no contract
	// finding.
	lp3 := standardProviders("v1.12.9", "v0.9.2", "", "")
	cu3 := ComponentUpgrade{CAPIVersion: "v1.13.0"}
	blocking3, _ := componentFindings(lp3, cu3, cu3.plan(lp3))
	if hasFinding(blocking3, "contract") {
		t.Fatalf("no contract finding expected within a contract: %v", blocking3)
	}
}

func TestSkewGuard(t *testing.T) {
	cases := []struct {
		name      string
		from      string
		to        string
		wantBlock bool
	}{
		{name: "one minor fine", from: "v1.12.9", to: "v1.13.0"},
		{name: "patch fine", from: "v1.12.9", to: "v1.12.10"},
		{name: "two minors blocked", from: "v1.11.0", to: "v1.13.0", wantBlock: true},
		{name: "downgrade blocked", from: "v1.13.0", to: "v1.12.0", wantBlock: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lp := standardProviders(tc.from, "v0.9.2", "", "")
			cu := ComponentUpgrade{CAPIVersion: tc.to}
			blocking, _ := componentFindings(lp, cu, cu.plan(lp))
			// Isolate skew findings (downgrade / minor) from any contract finding.
			got := hasFinding(blocking, "downgrade") || hasFinding(blocking, "minor versions above")
			if got != tc.wantBlock {
				t.Fatalf("skew blocking = %v, want %v: %v", got, tc.wantBlock, blocking)
			}
		})
	}
}

func TestWarningsAlwaysPresent(t *testing.T) {
	lp := standardProviders("v1.12.9", "v0.9.2", "", "")
	cu := ComponentUpgrade{CAPIVersion: "v1.13.0"}
	_, warnings := componentFindings(lp, cu, cu.plan(lp))
	if !hasFinding(warnings, "cert-manager") {
		t.Errorf("expected a cert-manager floor advisory: %v", warnings)
	}
	if !hasFindingObject(warnings, "self-managed cluster") {
		t.Errorf("expected the self-managed backup advisory: %v", warnings)
	}
}

func TestCAPICoreContract(t *testing.T) {
	for _, tc := range []struct{ v, want string }{
		{"v1.12.9", "v1beta2"},
		{"v1.11.0", "v1beta2"},
		{"v1.10.5", "v1beta1"},
		{"v1.5.0", "v1beta1"},
		{"v2.0.0", "v1beta2"},
		{"nonsense", ""},
	} {
		if got := capiCoreContract(tc.v); got != tc.want {
			t.Errorf("capiCoreContract(%q) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

// --- target CR rendering ----------------------------------------------------

func TestTargetProviderCRPreservesConfigSecret(t *testing.T) {
	infra := providerCR(generate.KindInfrastructureProvider, "capmox-system", "proxmox", "v0.9.2", "ghcr.io/me/capmox:v0.9.2-fix")
	_ = unstructured.SetNestedMap(infra.Object, map[string]any{
		"name": "proxmox-variables", "namespace": "capmox-system",
	}, "spec", "configSecret")
	p := componentPlan{
		live:        &liveProvider{kind: generate.KindInfrastructureProvider, obj: infra, version: "v0.9.2", image: "ghcr.io/me/capmox:v0.9.2-fix"},
		version:     "v0.10.0",
		image:       "ghcr.io/me/capmox:v0.10.0",
		versionBump: true,
	}
	out := targetProviderCR(p)
	if v, _, _ := unstructured.NestedString(out.Object, "spec", "version"); v != "v0.10.0" {
		t.Errorf("spec.version = %q, want v0.10.0", v)
	}
	if got := generate.ManagerImage(out); got != "ghcr.io/me/capmox:v0.10.0" {
		t.Errorf("manager image = %q, want the new image", got)
	}
	// The controller's credential reference must survive the upgrade.
	cs, found, _ := unstructured.NestedStringMap(out.Object, "spec", "configSecret")
	if !found || cs["name"] != "proxmox-variables" {
		t.Errorf("configSecret not preserved: found=%v %v", found, cs)
	}
	// Status is dropped so it is not re-applied.
	if _, found, _ := unstructured.NestedMap(out.Object, "status"); found {
		t.Error("status should be dropped from the applied object")
	}
}

// --- orchestration via the fake cluster -------------------------------------

func seedProviders(f *fakeCluster, lp *liveProviders) {
	for _, p := range []*liveProvider{lp.core, lp.bootstrap, lp.controlPlane, lp.infra} {
		if p != nil {
			f.objs[keyOf(p.obj)] = p.obj
		}
	}
}

func TestUpgradeComponentsOrder(t *testing.T) {
	deps, f := upgradeHarness(t)
	seedProviders(f, standardProviders("v1.12.9", "v0.9.2", "", ""))
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Components: ComponentUpgrade{CAPIVersion: "v1.13.0", ProviderVersion: "v0.10.0"},
	})
	if err != nil {
		t.Fatalf("component upgrade: %v", err)
	}
	// The four providers were applied in the safe order.
	want := []string{
		generate.KindCoreProvider + "/cluster-api",
		generate.KindBootstrapProvider + "/kubeadm",
		generate.KindControlPlaneProvider + "/kubeadm",
		generate.KindInfrastructureProvider + "/proxmox",
	}
	var got []string
	for _, a := range f.applied {
		for _, w := range want {
			if a == w {
				got = append(got, a)
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("applied %v, want all of %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("apply order = %v, want %v", got, want)
		}
	}
	// The stored core CR now carries the target version.
	core := f.objs[keyOf(providerCR(generate.KindCoreProvider, generate.CoreNamespace, generate.CoreProviderName, "", ""))]
	if v, _, _ := unstructured.NestedString(core.Object, "spec", "version"); v != "v1.13.0" {
		t.Errorf("core spec.version = %q, want v1.13.0", v)
	}
}

func TestUpgradeComponentsBlockedByForkImage(t *testing.T) {
	deps, f := upgradeHarness(t)
	// Core runs a fork image; a bare --capi-version bump must be refused.
	seedProviders(f, standardProviders("v1.12.9", "v0.9.2", "ghcr.io/klotho-k8s/cluster-api-controller:v1.12.9", ""))
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Components: ComponentUpgrade{CAPIVersion: "v1.13.0"},
	})
	var fe *UpgradeFindingsError
	if !errors.As(err, &fe) {
		t.Fatalf("want *UpgradeFindingsError, got %v", err)
	}
	if f.rec.has("kube.Apply klotho " + generate.KindCoreProvider) {
		t.Error("must not apply any provider when blocked by findings")
	}
}

func TestUpgradeComponentsForceOverrides(t *testing.T) {
	deps, f := upgradeHarness(t)
	seedProviders(f, standardProviders("v1.12.9", "v0.9.2", "ghcr.io/klotho-k8s/cluster-api-controller:v1.12.9", ""))
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Force:      true,
		Components: ComponentUpgrade{CAPIVersion: "v1.13.0", CoreImage: "ghcr.io/klotho-k8s/cluster-api-controller:v1.13.0"},
	})
	if err != nil {
		t.Fatalf("force component upgrade: %v", err)
	}
	if !f.rec.has("kube.Apply klotho " + generate.KindCoreProvider) {
		t.Error("core provider should be applied")
	}
}

func TestUpgradeComponentsNoProviders(t *testing.T) {
	deps, f := upgradeHarness(t)
	// No operator provider CRs seeded: discovery must fail clearly, not panic.
	_ = f
	err := Upgrade(context.Background(), deps, "demo", UpgradeOptions{
		Components: ComponentUpgrade{CAPIVersion: "v1.13.0"},
	})
	if err == nil || !contains(err.Error(), "CoreProvider") {
		t.Fatalf("want a no-CoreProvider error, got %v", err)
	}
}

func TestDiscoverProviders(t *testing.T) {
	rec := &recorder{}
	f := newFakeCluster(rec, "demo")
	seedProviders(f, standardProviders("v1.12.9", "v0.9.2", "", "ghcr.io/me/capmox:v0.9.2-fix"))
	lp, err := discoverProviders(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if lp.core == nil || lp.core.version != "v1.12.9" {
		t.Errorf("core = %+v", lp.core)
	}
	if lp.infra == nil || lp.infra.image != "ghcr.io/me/capmox:v0.9.2-fix" {
		t.Errorf("infra image not read back: %+v", lp.infra)
	}
}

// --- helpers ----------------------------------------------------------------

func hasFinding(fs []provider.Finding, substr string) bool {
	for _, f := range fs {
		if strings.Contains(f.Message, substr) {
			return true
		}
	}
	return false
}

func hasFindingObject(fs []provider.Finding, object string) bool {
	for _, f := range fs {
		if f.Object == object {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }
