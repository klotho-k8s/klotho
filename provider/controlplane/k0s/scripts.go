package k0s

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Paths on the bootstrap node.
const (
	// PKIDir is where k0s reads its PKI; files pre-seeded here are used
	// instead of self-signed ones.
	PKIDir = "/var/lib/k0s/pki"
	// K0sConfigPath is where k0s reads its ClusterConfig (k0smotron's default
	// GetK0sConfigPath()).
	K0sConfigPath = "/etc/k0s.yaml"
	// AdminKubeconfig is the admin kubeconfig k0s writes once the controller
	// is up.
	AdminKubeconfig = "/var/lib/k0s/pki/admin.conf"

	ManifestsDir   = "/run/klotho/manifests"
	InstallScript  = "/run/klotho/k0s-install.sh"
	PostInitScript = "/run/klotho/post-init.sh"
	K0sBin         = "/usr/local/bin/k0s"
)

// runBash returns the runcmd entry that runs a script klotho wrote under /run.
// Ubuntu mounts /run noexec, so the file's mode does not make it runnable;
// handing it to the interpreter does.
func runBash(path string) string { return "/bin/bash " + path }

// installScript downloads the k0s binary (if not present) for the given k0s
// version, installs the single-node controller with the control plane's args
// and the pre-seeded CAs and config, starts it, and waits for the API server.
// AUTOPILOT_HOSTNAME is set to the adopted machine's name, as k0smotron's own
// control-plane bootstrap does, so the node matches the Machine K0sControlPlane
// adopts.
func installScript(k0sVersion, machineName string, args, preStart, postStart []string) []byte {
	var b strings.Builder
	b.WriteString(`#!/bin/bash
# Brings up node zero as a single k0s controller, with the cluster CAs and
# config klotho pre-seeded, so k0smotron's K0sControlPlane can adopt it in
# place. Every step is safe to repeat.
set -euo pipefail

log() { echo "k0s-install: $*" >&2; }

`)
	fmt.Fprintf(&b, "K0S_VERSION=%s\n", shellQuote(k0sVersion))
	fmt.Fprintf(&b, "AUTOPILOT_HOSTNAME=%s\n", shellQuote(machineName))
	b.WriteString(`
download() {
  [ -x ` + K0sBin + ` ] && return 0
  log "downloading k0s $K0S_VERSION"
  if command -v curl >/dev/null 2>&1; then
    curl -sSLf "https://github.com/k0sproject/k0s/releases/download/${K0S_VERSION}/k0s-${K0S_VERSION}-amd64" -o ` + K0sBin + `
  elif command -v wget >/dev/null 2>&1; then
    wget -qO ` + K0sBin + ` "https://github.com/k0sproject/k0s/releases/download/${K0S_VERSION}/k0s-${K0S_VERSION}-amd64"
  else
    log "neither curl nor wget is available to download k0s"; exit 1
  fi
  chmod 0755 ` + K0sBin + `
}

download

if [ ! -f /etc/systemd/system/k0scontroller.service ] && ! ` + K0sBin + ` status >/dev/null 2>&1; then
  log "installing the k0s controller"
  AUTOPILOT_HOSTNAME="$AUTOPILOT_HOSTNAME" ` + K0sBin + ` install controller \
    --force --enable-dynamic-config --config ` + K0sConfigPath + ` \
    --env AUTOPILOT_HOSTNAME="$AUTOPILOT_HOSTNAME"`)
	for _, a := range args {
		b.WriteString(" \\\n    " + shellQuote(a))
	}
	b.WriteString(`
`)
	for _, c := range preStart {
		b.WriteString("  " + c + "\n")
	}
	b.WriteString(`  log "starting k0s"
  ` + K0sBin + ` start
`)
	for _, c := range postStart {
		b.WriteString("  " + c + "\n")
	}
	b.WriteString(`fi

log "waiting for the k0s API server"
for i in $(seq 1 120); do
  if ` + K0sBin + ` kubectl get --raw=/readyz >/dev/null 2>&1; then
    log "API server is ready"; break
  fi
  sleep 5
done

# Write an admin kubeconfig for the post-init steps.
install -d -m 0700 "$(dirname ` + AdminKubeconfig + `)"
` + K0sBin + ` kubeconfig admin > ` + AdminKubeconfig + ` 2>/dev/null || true
`)
	return []byte(b.String())
}

var postInitTemplate = template.Must(template.New("post-init").Parse(`#!/bin/bash
# Runs once the k0s API server answers. It labels node zero as a control plane
# (so the Cluster API operator's provider deployments, which select control
# plane nodes, schedule on it), installs cert-manager and the Cluster API
# operator, and seeds the cluster Secrets K0sControlPlane looks up. k0s ships
# its own CNI, so there is no CNI step. Every step is safe to repeat.
set -euo pipefail

export KUBECONFIG={{ .Kubeconfig }}
K0S={{ .K0sBin }}
SERVER={{ .Server }}

log() { echo "post-init: $*" >&2; }
kc() { "$K0S" kubectl "$@"; }

retry() {
  local attempts=$1 delay=$2 i
  shift 2
  for ((i = 1; i <= attempts; i++)); do
    if "$@"; then return 0; fi
    log "attempt $i/$attempts failed: $*"
    sleep "$delay"
  done
  log "giving up: $*"
  return 1
}

api_ready() { kc get --raw=/readyz >/dev/null; }

cert_manager_webhook_ready() {
  printf '%s\n' \
    'apiVersion: cert-manager.io/v1' \
    'kind: Issuer' \
    'metadata:' \
    '  name: post-init-webhook-probe' \
    '  namespace: cert-manager' \
    'spec:' \
    '  selfSigned: {}' |
    kc apply --dry-run=server -f - >/dev/null
}

log "waiting for the API server"
retry 120 5 api_ready

# Label node zero as a control plane so the operator's provider deployments
# (nodeSelector node-role.kubernetes.io/control-plane) schedule, and so the
# ordered self-managed deletion, which keeps providers on control planes, works.
log "labelling node zero as a control plane"
retry 60 5 bash -c 'n="$('"$K0S"' kubectl get nodes -o jsonpath={.items[0].metadata.name})"; [ -n "$n" ] && '"$K0S"' kubectl label node "$n" node-role.kubernetes.io/control-plane= --overwrite'
{{- if .ContainerNodes }}

# Set node zero's providerID so Cluster API links the adopted Machine to its
# node. The kubeadm bootstrap flow has CAPD patch the node providerID by
# exec-ing kubectl at kubeadm's path, which k0s does not provide; k0s's kubelet
# does not set a providerID on its own. The node name is the container name, so
# the DockerMachine providerID is docker:////<node>. Without this the Machine
# never gets a status.nodeRef and the control plane stays unavailable.
log "setting node zero providerID for CAPD"
retry 60 5 bash -c 'n="$('"$K0S"' kubectl get nodes -o jsonpath={.items[0].metadata.name})"; [ -n "$n" ] && '"$K0S"' kubectl patch node "$n" --type merge -p "{\"spec\":{\"providerID\":\"docker:////$n\"}}"'
{{- end }}

retry 60 10 kc wait --for=condition=Ready nodes --all --timeout=10s

log "applying cert-manager"
retry 30 10 kc apply -f {{ .CertManager }}
retry 60 10 kc -n cert-manager wait --for=condition=Available deployment --all --timeout=10s
retry 60 5 cert_manager_webhook_ready

log "applying the Cluster API operator"
retry 30 10 kc apply --server-side --force-conflicts -f {{ .Operator }}
retry 60 10 kc -n capi-operator-system wait --for=condition=Available deployment --all --timeout=10s

log "seeding the cluster Secrets"
retry 30 5 kc apply -f {{ .Manifests }}
{{- if .Extra }}

log "running extra post-init commands"
{{- range .Extra }}
{{ . }}
{{- end }}
{{- end }}

log "done"
`))

func (p *Provider) postInitScript(ep cluster.Endpoint) []byte {
	var b bytes.Buffer
	err := postInitTemplate.Execute(&b, map[string]any{
		"Kubeconfig":  shellQuote(AdminKubeconfig),
		"K0sBin":      K0sBin,
		"Server":      shellQuote(serverURL(ep)),
		"CertManager":    shellQuote(p.opts.CertManagerURL),
		"Operator":       shellQuote(p.opts.OperatorURL),
		"Manifests":      shellQuote(ManifestsDir + "/"),
		"Extra":          p.opts.ExtraPostCommands,
		"ContainerNodes": p.opts.ContainerNodes,
	})
	if err != nil {
		panic(err)
	}
	return b.Bytes()
}

// shellQuote quotes s as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
