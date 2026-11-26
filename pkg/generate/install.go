package generate

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"text/template"

	"github.com/blang/semver/v4"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// API groups of the bootstrap and control plane objects the install patch
// touches.
const (
	ControlPlaneGroup = "controlplane.cluster.x-k8s.io"
	BootstrapGroup    = "bootstrap.cluster.x-k8s.io"
)

// InstallScriptPath is where the install-at-boot script is written on every
// node. It lives outside /run (which Ubuntu mounts noexec) so its mode makes
// it runnable, and the pre-kubeadm command runs it by path.
const InstallScriptPath = "/usr/local/bin/klotho-install-kubernetes.sh"

// InstallLogPath is where the script logs on the node.
const InstallLogPath = "/var/log/klotho-install-kubernetes.log"

// InstallKubernetesOptions tune the install-at-boot script. All fields are
// optional.
type InstallKubernetesOptions struct {
	// AptProxy is an http(s) URL of an apt proxy such as apt-cacher-ng. The
	// script writes it to /etc/apt/apt.conf.d/01klotho-proxy.
	AptProxy string
	// RegistryMirrors maps a registry host (registry.k8s.io, docker.io, ...)
	// to the mirror URLs containerd pulls from first. The script writes a
	// hosts.toml per registry under /etc/containerd/certs.d.
	RegistryMirrors map[string][]string
	// Packages are installed and enabled before anything else, on every
	// node, prepared image or not: what the infrastructure provider needs to
	// see the node at all (see provider.PackageRequirer).
	Packages []string
}

// Validate checks the option URLs.
func (o InstallKubernetesOptions) Validate() error {
	if o.AptProxy != "" {
		if err := checkHTTPURL(o.AptProxy); err != nil {
			return fmt.Errorf("apt proxy: %w", err)
		}
	}
	for registry, mirrors := range o.RegistryMirrors {
		if strings.TrimSpace(registry) == "" || strings.ContainsAny(registry, "/ \t\"") {
			return fmt.Errorf("registry mirror key %q is not a registry host", registry)
		}
		if len(mirrors) == 0 {
			return fmt.Errorf("registry %s lists no mirrors", registry)
		}
		for _, m := range mirrors {
			if err := checkHTTPURL(m); err != nil {
				return fmt.Errorf("registry %s mirror: %w", registry, err)
			}
		}
	}
	return nil
}

func checkHTTPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%q must be an http(s) URL", s)
	}
	return nil
}

// SetInstallKubernetes makes every node install containerd and Kubernetes at
// boot: it adds the install script (InstallScriptPath, mode 0755) to the
// files of the KubeadmControlPlane's kubeadmConfigSpec and of every
// KubeadmConfigTemplate's spec.template.spec, puts the script first in their
// preKubeadmCommands (the user's commands keep their order after it), and
// re-renders b.YAML. The Kubernetes version is the KubeadmControlPlane's
// spec.version; every MachineDeployment must ask for the same one. Applying
// the patch twice leaves the bundle as after the first time.
func SetInstallKubernetes(b *Bundle, opts InstallKubernetesOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	kcp, err := FindOne(b, ControlPlaneGroup, "KubeadmControlPlane")
	if err != nil {
		return err
	}
	version, _, err := unstructured.NestedString(kcp.Object, "spec", "version")
	if err != nil {
		return errors.Wrapf(err, "reading version of %s %q", kcp.GetKind(), kcp.GetName())
	}
	v, err := parseKubernetesVersion(version)
	if err != nil {
		return fmt.Errorf("%s %q spec.version: %w", kcp.GetKind(), kcp.GetName(), err)
	}
	for _, md := range Find(b, ClusterGroup, "MachineDeployment") {
		mdv, _, err := unstructured.NestedString(md.Object, "spec", "template", "spec", "version")
		if err != nil {
			return errors.Wrapf(err, "reading version of %s %q", md.GetKind(), md.GetName())
		}
		if mdv != "" && mdv != version {
			return fmt.Errorf("%s %q asks for Kubernetes %s but %s %q for %s; the install script carries one version",
				md.GetKind(), md.GetName(), mdv, kcp.GetKind(), kcp.GetName(), version)
		}
	}

	script, err := InstallScript(v, opts)
	if err != nil {
		return err
	}
	if err := patchKubeadmSpec(kcp, script, "spec", "kubeadmConfigSpec"); err != nil {
		return err
	}
	for _, kct := range Find(b, BootstrapGroup, "KubeadmConfigTemplate") {
		if err := patchKubeadmSpec(kct, script, "spec", "template", "spec"); err != nil {
			return err
		}
	}
	_, err = b.Render()
	return err
}

func parseKubernetesVersion(s string) (semver.Version, error) {
	if s == "" {
		return semver.Version{}, errors.New("is empty")
	}
	v, err := semver.ParseTolerant(s)
	if err != nil {
		return semver.Version{}, fmt.Errorf("%q is not a Kubernetes version: %w", s, err)
	}
	return v, nil
}

// patchKubeadmSpec adds the script file and the pre-kubeadm command to the
// KubeadmConfigSpec at path inside obj.
func patchKubeadmSpec(obj *unstructured.Unstructured, script []byte, path ...string) error {
	spec, _, err := unstructured.NestedMap(obj.Object, path...)
	if err != nil {
		return errors.Wrapf(err, "reading %s of %s %q", strings.Join(path, "."), obj.GetKind(), obj.GetName())
	}
	if spec == nil {
		spec = map[string]any{}
	}

	file := map[string]any{
		"path":        InstallScriptPath,
		"owner":       "root:root",
		"permissions": "0755",
		"content":     string(script),
	}
	var files []any
	if existing, ok := spec["files"].([]any); ok {
		for _, f := range existing {
			if m, ok := f.(map[string]any); ok && m["path"] == InstallScriptPath {
				continue
			}
			files = append(files, f)
		}
	}
	spec["files"] = append([]any{file}, files...)

	pre := []any{InstallScriptPath}
	if existing, ok := spec["preKubeadmCommands"].([]any); ok {
		for _, c := range existing {
			if c == InstallScriptPath {
				continue
			}
			pre = append(pre, c)
		}
	}
	spec["preKubeadmCommands"] = pre

	if err := unstructured.SetNestedMap(obj.Object, spec, path...); err != nil {
		return errors.Wrapf(err, "setting %s of %s %q", strings.Join(path, "."), obj.GetKind(), obj.GetName())
	}
	return nil
}

// InstallScript renders the install-at-boot script for Kubernetes version v.
func InstallScript(v semver.Version, opts InstallKubernetesOptions) ([]byte, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	type mirror struct {
		Registry string
		Server   string
		Mirrors  []string
	}
	var mirrors []mirror
	for registry, urls := range opts.RegistryMirrors {
		server := "https://" + registry
		if registry == "docker.io" {
			server = "https://registry-1.docker.io"
		}
		mirrors = append(mirrors, mirror{Registry: registry, Server: server, Mirrors: urls})
	}
	sort.Slice(mirrors, func(i, j int) bool { return mirrors[i].Registry < mirrors[j].Registry })

	var buf bytes.Buffer
	err := installScriptTemplate.Execute(&buf, map[string]any{
		"Version":  fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch),
		"Minor":    fmt.Sprintf("v%d.%d", v.Major, v.Minor),
		"AptProxy": opts.AptProxy,
		"Packages": opts.Packages,
		"Mirrors":  mirrors,
		"Log":      InstallLogPath,
	})
	if err != nil {
		return nil, fmt.Errorf("rendering install script: %w", err)
	}
	return buf.Bytes(), nil
}

var installScriptTemplate = template.Must(template.New("install").Parse(`#!/usr/bin/env bash
# Installs containerd and Kubernetes {{.Version}} on a plain Debian or Ubuntu
# image at boot. Written into the cluster bundle by klotho's install-kubernetes
# option and run before kubeadm on every node. Idempotent: a node that already
# has kubeadm {{.Version}} (a prepared template) exits at once.
set -euo pipefail

WANT_VERSION="{{.Version}}"
WANT_MINOR="{{.Minor}}"
APT_PROXY="{{.AptProxy}}"
LOG="{{.Log}}"
export DEBIAN_FRONTEND=noninteractive

exec > >(tee -a "$LOG") 2>&1
log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) klotho-install-kubernetes: $*"; }

if [ -n "$APT_PROXY" ]; then
  log "using apt proxy $APT_PROXY"
  cat > /etc/apt/apt.conf.d/01klotho-proxy <<EOF
Acquire::http::Proxy "$APT_PROXY";
Acquire::https::Proxy "$APT_PROXY";
EOF
fi
{{- if .Packages}}

# Packages the infrastructure provider needs before anything else (an agent it
# reads node addresses from). Checked on every node, prepared image or not.
for p in{{range .Packages}} {{.}}{{end}}; do
  if ! dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q "install ok installed"; then
    log "installing provider package $p"
    for n in 1 2 3 4 5; do
      if apt-get update && apt-get install -y --no-install-recommends "$p"; then break; fi
      log "attempt $n of 5 failed for $p; retrying in $((n * 10))s"; sleep $((n * 10))
    done
    systemctl enable --now "$p" >/dev/null 2>&1 || true
  fi
done
{{- end}}

if command -v kubeadm >/dev/null 2>&1; then
  have="$(kubeadm version -o short 2>/dev/null || true)"
  if [ "${have#v}" = "$WANT_VERSION" ]; then
    log "kubeadm v$WANT_VERSION is already installed; nothing to do"
    exit 0
  fi
  log "kubeadm ${have:-of unknown version} is installed, want v$WANT_VERSION"
fi
log "installing containerd and Kubernetes v$WANT_VERSION"

# Networks flap while a node boots: give every apt and download step a few tries.
retry() {
  local n
  for n in 1 2 3 4 5; do
    if "$@"; then
      return 0
    fi
    log "attempt $n of 5 failed: $*; retrying in $((n * 10))s"
    sleep $((n * 10))
  done
  log "giving up on: $*"
  return 1
}

log "kernel modules and sysctls"
cat > /etc/modules-load.d/klotho-kubernetes.conf <<'EOF'
overlay
br_netfilter
EOF
modprobe overlay
modprobe br_netfilter
cat > /etc/sysctl.d/99-klotho-kubernetes.conf <<'EOF'
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system >/dev/null

log "disabling swap"
swapoff -a || true
sed -i -E 's/^([^#].*[[:space:]]swap[[:space:]].*)$/#\1/' /etc/fstab

log "installing containerd and kubeadm's runtime dependencies from the distribution"
# conntrack is a fatal kubeadm preflight requirement and socat is needed for
# port-forward; neither ships in the generic cloud images, so install them here
# or kubeadm init aborts at preflight ("conntrack not found in system path").
retry apt-get update
# conntrack, socat and ethtool are kubeadm preflight requirements that a plain
# distribution cloud image (an AWS Ubuntu AMI, for example) does not ship;
# without conntrack kubeadm init aborts at preflight.
retry apt-get install -y --no-install-recommends ca-certificates curl gpg apt-transport-https containerd conntrack socat ethtool
mkdir -p /etc/containerd
containerd config default > /etc/containerd/config.toml
sed -i -E 's/(SystemdCgroup\s*=\s*)false/\1true/' /etc/containerd/config.toml
{{- if .Mirrors}}

log "configuring containerd registry mirrors"
sed -i -E 's#^(\s*config_path\s*=\s*).*$#\1"/etc/containerd/certs.d"#' /etc/containerd/config.toml
if ! grep -q '/etc/containerd/certs.d' /etc/containerd/config.toml; then
  log "WARNING: containerd config has no registry config_path setting; mirrors are not active"
fi
{{- range .Mirrors}}
mkdir -p "/etc/containerd/certs.d/{{.Registry}}"
cat > "/etc/containerd/certs.d/{{.Registry}}/hosts.toml" <<'EOF'
server = "{{.Server}}"
{{range .Mirrors}}
[host."{{.}}"]
  capabilities = ["pull", "resolve"]
{{end}}EOF
{{- end}}
{{- end}}

log "adding the Kubernetes $WANT_MINOR package repository"
install -m 0755 -d /etc/apt/keyrings
retry curl -fsSL "https://pkgs.k8s.io/core:/stable:/$WANT_MINOR/deb/Release.key" -o /tmp/klotho-kubernetes-release.key
gpg --dearmor --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg /tmp/klotho-kubernetes-release.key
rm -f /tmp/klotho-kubernetes-release.key
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/$WANT_MINOR/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
retry apt-get update

apt-mark unhold kubelet kubeadm kubectl >/dev/null 2>&1 || true
if apt-cache madison kubeadm | grep -q " ${WANT_VERSION}-"; then
  log "installing kubelet, kubeadm and kubectl ${WANT_VERSION}"
  retry apt-get install -y --allow-downgrades "kubelet=${WANT_VERSION}-*" "kubeadm=${WANT_VERSION}-*" "kubectl=${WANT_VERSION}-*"
else
  log "WARNING: no ${WANT_VERSION} packages in the $WANT_MINOR repository; installing its latest release instead"
  retry apt-get install -y kubelet kubeadm kubectl
fi
apt-mark hold kubelet kubeadm kubectl

log "configuring the pause image kubeadm expects"
PAUSE="$(kubeadm config images list --kubernetes-version "v$WANT_VERSION" 2>/dev/null | grep '/pause:' | head -n 1 || true)"
if [ -n "$PAUSE" ]; then
  sed -i -E "s#^(\s*sandbox_image\s*=\s*).*\$#\1\"$PAUSE\"#; s#^(\s*sandbox\s*=\s*).*\$#\1\"$PAUSE\"#" /etc/containerd/config.toml
else
  log "WARNING: could not determine the pause image; containerd keeps its default"
fi
systemctl enable containerd >/dev/null 2>&1 || true
systemctl restart containerd
systemctl enable kubelet >/dev/null 2>&1 || true

log "done: $(kubeadm version -o short), containerd $(containerd --version | awk '{print $3}')"
`))
