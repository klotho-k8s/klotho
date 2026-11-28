package k3s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/controlplane"
)

// UserData implements controlplane.Provider. It renders node zero's cloud-init:
// the cluster CAs in k3s's PKI layout, the k3s server config.yaml with the
// control plane endpoint and a fixed cluster token, the kthreesConfigSpec.files,
// the install-and-start script, the cluster Secret manifests KThreesControlPlane
// looks up (including the token), and the post-init script.
func (p *Provider) UserData(_ context.Context, spec *cluster.Spec, creds *controlplane.Credentials, cp, clusterObj *unstructured.Unstructured) (*controlplane.Contribution, error) {
	if spec == nil || spec.Name == "" {
		return nil, errors.New("k3s: cluster name is required")
	}
	if creds == nil {
		return nil, errors.New("k3s: credentials are required")
	}
	kcp, err := p.decodeControlPlane(cp)
	if err != nil {
		return nil, err
	}
	if kcp.version == "" {
		return nil, errors.New("k3s: a version is required (KThreesControlPlane spec.version)")
	}
	ep, err := endpointFromKubeconfig(creds.Kubeconfig)
	if err != nil {
		return nil, err
	}
	nw, err := p.network(clusterObj)
	if err != nil {
		return nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}

	var c controlplane.Contribution
	addFile := func(path string, content []byte, mode string) {
		c.Files = append(c.Files, controlplane.File{Path: path, Content: content, Mode: mode, Owner: "root:root"})
	}

	// The cluster CAs in k3s's PKI layout, so k3s uses them instead of
	// self-signing. The k3s bootstrap provider writes the same files from the
	// same CA Secrets when it bootstraps a control plane node.
	c.Files = append(c.Files, nodeCAFiles(creds)...)

	// The k3s server config, with the control plane endpoint, cluster token and
	// networking injected.
	configYAML, err := p.configYAML(kcp, ep, nw, token, BootstrapMachineName(spec.Name))
	if err != nil {
		return nil, err
	}
	addFile(ConfigPath, configYAML, "0640")

	// kthreesConfigSpec.files, written as the k3s bootstrap provider writes them
	// on the machines it bootstraps.
	for _, f := range kcp.files {
		mode := f.permissions
		if mode == "" {
			mode = "0644"
		}
		owner := f.owner
		if owner == "" {
			owner = "root:root"
		}
		c.Files = append(c.Files, controlplane.File{Path: f.path, Content: []byte(f.content), Mode: mode, Owner: owner})
	}

	// The cluster Secrets KThreesControlPlane looks up (<name>-ca, -cca, -etcd,
	// -token, -kubeconfig), applied by the post-init script.
	manifests, err := seedManifests(spec.Name, namespace(spec), creds, token, creds.Kubeconfig)
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		addFile(ManifestsDir+"/"+m.name, m.body, "0600")
	}

	addFile(InstallScript, installScript(kcp.version, kcp.postCommands), "0700")
	addFile(PostInitScript, p.postInitScript(), "0700")

	c.PreCommands = append(c.PreCommands, kcp.preCommands...)
	c.InstallCommands = []string{runBash(InstallScript)}
	c.PostCommands = []string{runBash(PostInitScript)}
	return &c, nil
}

// configYAML renders node zero's /etc/rancher/k3s/config.yaml. cluster-init:
// true makes k3s the first embedded-etcd server (k3s otherwise defaults to the
// sqlite datastore, which has no etcd CA to seed), so the seeded etcd CA is the
// one k3s uses and KThreesControlPlane adopts it without regenerating. The
// token is fixed so the seeded <name>-token Secret, which later joiners read,
// matches node zero.
//
// For container nodes (CAPD): kube-proxy's conntrack sysctl writes and the
// kubelet's swap check are disabled, exactly as the kubeadm provider does for
// kind nodes. k3s's embedded cloud-controller is disabled and the kubelet is
// given an explicit provider-id of docker:////<node>, because CAPD links the
// adopted Machine to its Node by that provider ID (the node name equals the
// container name); left to run, k3s's cloud-controller sets providerID
// k3s://<node>, which never matches the DockerMachine and is immutable once
// written, so the Machine would never get a nodeRef.
func (p *Provider) configYAML(kcp *controlPlane, ep cluster.Endpoint, nw network, token, nodeName string) ([]byte, error) {
	cfg := map[string]any{
		"cluster-init": true,
		"token":        token,
	}

	sans := []string{ep.Host, "127.0.0.1", "localhost"}
	for _, s := range toStringSlice(kcp.serverConfig["tlsSan"]) {
		if !containsStr(sans, s) {
			sans = append(sans, s)
		}
	}
	cfg["tls-san"] = sans

	if nw.pods != "" {
		cfg["cluster-cidr"] = nw.pods
	}
	if nw.services != "" {
		cfg["service-cidr"] = nw.services
	}

	// serverConfig.disableComponents maps to k3s's --disable for the packaged
	// components (traefik, servicelb, metrics-server, local-storage, coredns).
	if plugins := toStringSlice(kcp.serverConfig["disableComponents"]); len(plugins) > 0 {
		cfg["disable"] = plugins
	}

	if p.opts.ContainerNodes {
		// kube-proxy cannot size the conntrack table inside a rootless,
		// read-only /proc/sys container; zeroing maxPerCore skips the write.
		cfg["kube-proxy-arg"] = []string{"conntrack-max-per-core=0"}
		// Disable k3s's cloud-controller so the node is not tainted
		// uninitialized and so it does not set a k3s:// provider ID; give the
		// kubelet the CAPD provider ID instead, so the node registers with the
		// ID CAPD expects from the start.
		cfg["disable-cloud-controller"] = true
		cfg["kubelet-arg"] = []string{
			"fail-swap-on=false",
			"provider-id=docker:////" + nodeName,
		}
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("k3s: rendering config.yaml: %w", err)
	}
	return out, nil
}

// newToken returns a fresh k3s cluster token: 32 hex characters, the form the
// k3s bootstrap provider generates.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("k3s: generating cluster token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
