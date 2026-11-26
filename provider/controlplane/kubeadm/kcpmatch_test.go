package kubeadm

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/blang/semver/v4"

	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	"sigs.k8s.io/cluster-api/bootstrap/kubeadm/defaulting"
)

// kcpRolloutNeeded reproduces KubeadmControlPlane's KubeadmConfig rollout
// check for one machine (matchesKubeadmConfig in the KubeadmControlPlane
// controller's filters.go, cluster-api v1.12.9), which lives in an internal
// package and cannot be imported:
//
//  1. Compute the desired KubeadmConfig for a joining machine, convert the
//     current config's initConfiguration into a joinConfiguration if it is
//     an init config, normalise both, and compare.
//  2. If that differs and the current config is an init config, compute the
//     desired config for an init machine and compare without conversion.
//
// Both the KubeadmConfig and the KubeadmControlPlane go through a JSON round
// trip first, as they would when stored by the API server.
func kcpRolloutNeeded(t *testing.T, kcp *controlplanev1.KubeadmControlPlane, current *bootstrapv1.KubeadmConfig) (bool, string) {
	t.Helper()
	kcp = roundTrip(t, kcp)
	current = roundTrip(t, current)

	desired := desiredKubeadmConfig(t, kcp, true)
	d, c := prepareForDiff(desired, current, true)
	if reflect.DeepEqual(d.Spec, c.Spec) {
		return false, ""
	}
	why := "join comparison:\n got: " + toYAML(t, c.Spec) + "\nwant: " + toYAML(t, d.Spec)
	if current.Spec.JoinConfiguration.ControlPlane == nil {
		desired = desiredKubeadmConfig(t, kcp, false)
		d, c = prepareForDiff(desired, current, false)
		if reflect.DeepEqual(d.Spec, c.Spec) {
			return false, ""
		}
		why += "\ninit comparison:\n got: " + toYAML(t, c.Spec) + "\nwant: " + toYAML(t, d.Spec)
	}
	return true, why
}

func roundTrip[T any](t *testing.T, in *T) *T {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := new(T)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func desiredKubeadmConfig(t *testing.T, kcp *controlplanev1.KubeadmControlPlane, isJoin bool) *bootstrapv1.KubeadmConfig {
	t.Helper()
	spec := kcp.Spec.KubeadmConfigSpec.DeepCopy()
	if isJoin {
		spec.InitConfiguration = bootstrapv1.InitConfiguration{}
		if spec.JoinConfiguration.ControlPlane == nil {
			spec.JoinConfiguration.ControlPlane = &bootstrapv1.JoinControlPlane{}
		}
	} else {
		spec.JoinConfiguration = bootstrapv1.JoinConfiguration{}
	}
	v, err := semver.ParseTolerant(kcp.Spec.Version)
	if err != nil {
		t.Fatal(err)
	}
	v.Pre = nil
	if v.GTE(semver.MustParse("1.31.0")) && v.LT(semver.MustParse("1.36.0")) {
		if spec.ClusterConfiguration.FeatureGates == nil {
			spec.ClusterConfiguration.FeatureGates = map[string]bool{}
		}
		if _, ok := spec.ClusterConfiguration.FeatureGates["ControlPlaneKubeletLocalMode"]; !ok {
			spec.ClusterConfiguration.FeatureGates["ControlPlaneKubeletLocalMode"] = true
		}
	}
	return &bootstrapv1.KubeadmConfig{Spec: *spec}
}

func prepareForDiff(desired, current *bootstrapv1.KubeadmConfig, convertInitToJoin bool) (*bootstrapv1.KubeadmConfig, *bootstrapv1.KubeadmConfig) {
	desired = desired.DeepCopy()
	current = current.DeepCopy()

	if convertInitToJoin && current.Spec.JoinConfiguration.ControlPlane == nil {
		init := current.Spec.InitConfiguration
		current.Spec.JoinConfiguration.Timeouts = init.Timeouts
		current.Spec.JoinConfiguration.Patches = init.Patches
		current.Spec.JoinConfiguration.SkipPhases = init.SkipPhases
		current.Spec.JoinConfiguration.NodeRegistration = init.NodeRegistration
		current.Spec.JoinConfiguration.ControlPlane = &bootstrapv1.JoinControlPlane{LocalAPIEndpoint: init.LocalAPIEndpoint}
		current.Spec.InitConfiguration = bootstrapv1.InitConfiguration{}
		current.Spec.JoinConfiguration.CACertPath = desired.Spec.JoinConfiguration.CACertPath
	}

	current.Spec.ClusterConfiguration.ControlPlaneEndpoint = desired.Spec.ClusterConfiguration.ControlPlaneEndpoint
	current.Spec.ClusterConfiguration.DNS = desired.Spec.ClusterConfiguration.DNS

	defaulting.ApplyPreviousKubeadmConfigDefaults(&desired.Spec)
	defaulting.ApplyPreviousKubeadmConfigDefaults(&current.Spec)

	desired.Spec.JoinConfiguration.Discovery = bootstrapv1.Discovery{}
	current.Spec.JoinConfiguration.Discovery = bootstrapv1.Discovery{}

	for _, s := range []*bootstrapv1.KubeadmConfigSpec{&desired.Spec, &current.Spec} {
		s.InitConfiguration.Timeouts.ControlPlaneComponentHealthCheckSeconds = nil
		s.JoinConfiguration.Timeouts.ControlPlaneComponentHealthCheckSeconds = nil
		dropEmpty(s)
	}
	return desired, current
}

// dropEmpty treats nil and empty collections alike, as KubeadmControlPlane
// does before comparing.
func dropEmpty(s *bootstrapv1.KubeadmConfigSpec) {
	cc := &s.ClusterConfiguration
	nilIfEmpty(&cc.Etcd.Local.ExtraArgs)
	nilIfEmpty(&cc.Etcd.Local.ServerCertSANs)
	nilIfEmpty(&cc.Etcd.Local.PeerCertSANs)
	nilIfEmpty(&cc.APIServer.ExtraArgs)
	nilIfEmpty(&cc.APIServer.ExtraVolumes)
	nilIfEmpty(&cc.APIServer.CertSANs)
	nilIfEmpty(&cc.ControllerManager.ExtraArgs)
	nilIfEmpty(&cc.ControllerManager.ExtraVolumes)
	nilIfEmpty(&cc.Scheduler.ExtraArgs)
	nilIfEmpty(&cc.Scheduler.ExtraVolumes)
	for _, envs := range []**[]bootstrapv1.EnvVar{&cc.Etcd.Local.ExtraEnvs, &cc.APIServer.ExtraEnvs, &cc.ControllerManager.ExtraEnvs, &cc.Scheduler.ExtraEnvs} {
		if *envs != nil && len(**envs) == 0 {
			*envs = nil
		}
	}
	if len(cc.FeatureGates) == 0 {
		cc.FeatureGates = nil
	}
	nilIfEmpty(&s.InitConfiguration.BootstrapTokens)
	for i := range s.InitConfiguration.BootstrapTokens {
		nilIfEmpty(&s.InitConfiguration.BootstrapTokens[i].Usages)
		nilIfEmpty(&s.InitConfiguration.BootstrapTokens[i].Groups)
	}
	for _, nr := range []*bootstrapv1.NodeRegistrationOptions{&s.InitConfiguration.NodeRegistration, &s.JoinConfiguration.NodeRegistration} {
		nilIfEmpty(&nr.KubeletExtraArgs)
		nilIfEmpty(&nr.IgnorePreflightErrors)
	}
	nilIfEmpty(&s.InitConfiguration.SkipPhases)
	nilIfEmpty(&s.JoinConfiguration.SkipPhases)
	nilIfEmpty(&s.JoinConfiguration.Discovery.BootstrapToken.CACertHashes)
	nilIfEmpty(&s.Files)
	nilIfEmpty(&s.DiskSetup.Filesystems)
	nilIfEmpty(&s.DiskSetup.Partitions)
	nilIfEmpty(&s.Mounts)
	nilIfEmpty(&s.BootCommands)
	nilIfEmpty(&s.PreKubeadmCommands)
	nilIfEmpty(&s.PostKubeadmCommands)
	nilIfEmpty(&s.Users)
	for i := range s.Users {
		nilIfEmpty(&s.Users[i].SSHAuthorizedKeys)
	}
	nilIfEmpty(&s.NTP.Servers)
}

func nilIfEmpty[T any](s *[]T) {
	if len(*s) == 0 {
		*s = nil
	}
}
