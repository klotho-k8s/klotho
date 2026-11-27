package rke2

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
// the cluster CAs in rke2's PKI layout, the rke2 server config.yaml with the
// control plane endpoint and a fixed cluster token, the spec.files, the
// install-and-start script, the cluster Secret manifests RKE2ControlPlane looks
// up (including the token), and the post-init script.
func (p *Provider) UserData(_ context.Context, spec *cluster.Spec, creds *controlplane.Credentials, cp, clusterObj *unstructured.Unstructured) (*controlplane.Contribution, error) {
	if spec == nil || spec.Name == "" {
		return nil, errors.New("rke2: cluster name is required")
	}
	if creds == nil {
		return nil, errors.New("rke2: credentials are required")
	}
	rcp, err := p.decodeControlPlane(cp)
	if err != nil {
		return nil, err
	}
	if rcp.version == "" {
		return nil, errors.New("rke2: a version is required (RKE2ControlPlane spec.version)")
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

	// The cluster CAs in rke2's PKI layout, so rke2 uses them instead of
	// self-signing. The RKE2 bootstrap provider writes the same files from the
	// same CA Secrets when it bootstraps a control plane node.
	c.Files = append(c.Files, nodeCAFiles(creds)...)

	// The rke2 server config, with the control plane endpoint, cluster token
	// and networking injected.
	configYAML, err := p.configYAML(rcp, ep, nw, token)
	if err != nil {
		return nil, err
	}
	addFile(ConfigPath, configYAML, "0640")

	// spec.files, written as the RKE2 bootstrap provider writes them on the
	// machines it bootstraps.
	for _, f := range rcp.files {
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

	// The cluster Secrets RKE2ControlPlane looks up (<name>-ca, -cca,
	// -peer-etcd, -etcd, -token, -kubeconfig), applied by the post-init script.
	manifests, err := seedManifests(spec.Name, namespace(spec), creds, token, creds.Kubeconfig)
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		addFile(ManifestsDir+"/"+m.name, m.body, "0600")
	}

	addFile(InstallScript, installScript(rcp.version, rcp.postCommands), "0700")
	addFile(PostInitScript, p.postInitScript(), "0700")

	c.PreCommands = append(c.PreCommands, rcp.preCommands...)
	c.InstallCommands = []string{runBash(InstallScript)}
	c.PostCommands = []string{runBash(PostInitScript)}
	return &c, nil
}

// configYAML renders node zero's /etc/rancher/rke2/config.yaml. The absence of
// a "server" key makes rke2 the cluster-init server, as the RKE2 bootstrap
// provider's init config does. The token is fixed so the seeded <name>-token
// Secret, which later joiners read, matches node zero. For container nodes
// (CAPD) kube-proxy's conntrack sysctl writes and the kubelet's swap check are
// disabled, exactly as the kubeadm provider does for kind nodes.
func (p *Provider) configYAML(rcp *controlPlane, ep cluster.Endpoint, nw network, token string) ([]byte, error) {
	cfg := map[string]any{"token": token}

	sans := []string{ep.Host, "127.0.0.1", "localhost"}
	for _, s := range toStringSlice(rcp.serverConfig["tlsSan"]) {
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

	// serverConfig: CNI and disabled components. The API's cni is a scalar
	// string (none/calico/canal/cilium); rke2's config.yaml takes it as is.
	switch cni := rcp.serverConfig["cni"].(type) {
	case string:
		if cni != "" {
			cfg["cni"] = cni
		}
	case []any:
		if s := toStringSlice(cni); len(s) > 0 {
			cfg["cni"] = s
		}
	}
	if dc, ok := rcp.serverConfig["disableComponents"].(map[string]any); ok {
		if plugins := toStringSlice(dc["pluginComponents"]); len(plugins) > 0 {
			cfg["disable"] = plugins
		}
		for _, comp := range toStringSlice(dc["kubernetesComponents"]) {
			switch comp {
			case "kubeProxy":
				cfg["disable-kube-proxy"] = true
			case "scheduler":
				cfg["disable-scheduler"] = true
			case "cloudController":
				cfg["disable-cloud-controller"] = true
			}
		}
	}

	if p.opts.ContainerNodes {
		// kube-proxy cannot size the conntrack table inside a rootless,
		// read-only /proc/sys container; zeroing maxPerCore skips the write.
		if _, disabled := cfg["disable-kube-proxy"]; !disabled {
			cfg["kube-proxy-arg"] = []string{"conntrack-max-per-core=0"}
		}
		cfg["kubelet-arg"] = []string{"fail-swap-on=false"}
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("rke2: rendering config.yaml: %w", err)
	}
	return out, nil
}

// newToken returns a fresh rke2 cluster token: 32 hex characters, the form the
// RKE2 bootstrap provider generates.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rke2: generating cluster token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
