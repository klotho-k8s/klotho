package k3s

import (
	"bytes"
	"strings"
	"text/template"
)

// runBash returns the runcmd entry that runs a script klotho wrote under /run.
// Ubuntu mounts /run noexec, so the file's mode does not make it runnable;
// handing it to the interpreter does.
func runBash(path string) string { return "/bin/bash " + path }

var installTemplate = template.Must(template.New("k3s-install").Parse(`#!/bin/bash
# Brings up node zero as a single k3s server, with the cluster CAs and config
# klotho pre-seeded (written before this runs), so cluster-api-k3s's
# KThreesControlPlane can adopt it in place. Every step is safe to repeat.
# cluster-init in config.yaml makes this the first embedded-etcd server.
set -euo pipefail

log() { echo "k3s-install: $*" >&2; }

K3S_VERSION={{ .Version }}

if ! systemctl is-active --quiet k3s.service; then
  if [ ! -x /usr/local/bin/k3s ]; then
    log "installing k3s $K3S_VERSION"
    if command -v curl >/dev/null 2>&1; then
      curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="$K3S_VERSION" INSTALL_K3S_SKIP_START=true sh -s - server
    else
      log "curl is required to install k3s"; exit 1
    fi
  fi
  log "enabling and starting k3s"
  systemctl enable k3s.service
  systemctl start k3s.service
fi
{{- range .PostCommands }}
{{ . }}
{{- end }}

export KUBECONFIG={{ .Kubeconfig }}
export PATH="{{ .BinDir }}:$PATH"
log "waiting for the k3s API server"
for i in $(seq 1 180); do
  if kubectl get --raw=/readyz >/dev/null 2>&1; then
    log "API server is ready"; break
  fi
  sleep 5
done
`))

func installScript(version string, postCommands []string) []byte {
	var b bytes.Buffer
	err := installTemplate.Execute(&b, map[string]any{
		"Version":      shellQuote(version),
		"PostCommands": postCommands,
		"Kubeconfig":   shellQuote(K3sKubeconfig),
		"BinDir":       BinDir,
	})
	if err != nil {
		panic(err)
	}
	return b.Bytes()
}

var postInitTemplate = template.Must(template.New("post-init").Parse(`#!/bin/bash
# Runs once the k3s API server answers. It makes sure node zero is a labelled,
# Ready control plane, installs cert-manager and the Cluster API operator, and
# seeds the cluster Secrets KThreesControlPlane looks up. k3s ships its own CNI
# (flannel), so there is no CNI step. Every step is safe to repeat.
set -euo pipefail

export KUBECONFIG={{ .Kubeconfig }}
export PATH="{{ .BinDir }}:$PATH"

log() { echo "post-init: $*" >&2; }

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

api_ready() { kubectl get --raw=/readyz >/dev/null; }

node_registered() { [ "$(kubectl get nodes --no-headers 2>/dev/null | wc -l)" -ge 1 ]; }

cert_manager_webhook_ready() {
  printf '%s\n' \
    'apiVersion: cert-manager.io/v1' \
    'kind: Issuer' \
    'metadata:' \
    '  name: post-init-webhook-probe' \
    '  namespace: cert-manager' \
    'spec:' \
    '  selfSigned: {}' |
    kubectl apply --dry-run=server -f - >/dev/null
}

log "waiting for the API server"
retry 180 5 api_ready

# Wait for node zero to register before touching it; k3s can answer /readyz
# before the kubelet has created the Node object.
log "waiting for node zero to register"
retry 120 5 node_registered
NODE="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
log "node zero is ${NODE}"

# k3s already labels its server nodes node-role.kubernetes.io/control-plane
# (value "true"); klotho's management stack selects that label by existence, so
# no relabelling is needed. The kubelet already carries the provider ID on
# container nodes. Defensively clear the cloud-provider uninitialized taint in
# case it was applied; best-effort, never aborts the bootstrap.
kubectl taint node "$NODE" node.cloudprovider.kubernetes.io/uninitialized- 2>/dev/null || true

log "waiting for node zero to be Ready"
retry 120 10 kubectl wait --for=condition=Ready "node/${NODE}" --timeout=10s

log "applying cert-manager"
retry 30 10 kubectl apply -f {{ .CertManager }}
retry 60 10 kubectl -n cert-manager wait --for=condition=Available deployment --all --timeout=10s
retry 60 5 cert_manager_webhook_ready

log "applying the Cluster API operator"
retry 30 10 kubectl apply --server-side --force-conflicts -f {{ .Operator }}
retry 60 10 kubectl -n capi-operator-system wait --for=condition=Available deployment --all --timeout=10s

log "seeding the cluster Secrets"
retry 30 5 kubectl apply -f {{ .Manifests }}
{{- if .Extra }}

log "running extra post-init commands"
{{- range .Extra }}
{{ . }}
{{- end }}
{{- end }}

log "done"
`))

func (p *Provider) postInitScript() []byte {
	var b bytes.Buffer
	err := postInitTemplate.Execute(&b, map[string]any{
		"Kubeconfig":     shellQuote(K3sKubeconfig),
		"BinDir":         BinDir,
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
