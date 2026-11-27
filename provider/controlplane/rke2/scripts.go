package rke2

import (
	"bytes"
	"strings"
	"text/template"
)

// runBash returns the runcmd entry that runs a script klotho wrote under /run.
// Ubuntu mounts /run noexec, so the file's mode does not make it runnable;
// handing it to the interpreter does.
func runBash(path string) string { return "/bin/bash " + path }

var installTemplate = template.Must(template.New("rke2-install").Parse(`#!/bin/bash
# Brings up node zero as a single rke2 server, with the cluster CAs and config
# klotho pre-seeded (written before this runs), so cluster-api-provider-rke2's
# RKE2ControlPlane can adopt it in place. Every step is safe to repeat. The
# absence of a "server:" key in config.yaml makes this the cluster-init server.
set -euo pipefail

log() { echo "rke2-install: $*" >&2; }

RKE2_VERSION={{ .Version }}

if ! systemctl is-active --quiet rke2-server.service; then
  if [ ! -x /usr/local/bin/rke2 ] && [ ! -x /opt/rke2/bin/rke2 ]; then
    log "installing rke2 $RKE2_VERSION"
    if command -v curl >/dev/null 2>&1; then
      curl -sfL https://get.rke2.io | INSTALL_RKE2_VERSION="$RKE2_VERSION" sh -s - server
    else
      log "curl is required to install rke2"; exit 1
    fi
  fi
  # rke2-agent is installed alongside the server unit; mask it so it never
  # races the server, as the RKE2 bootstrap provider's init cloud-init does.
  systemctl mask rke2-agent.service || true
  log "enabling and starting rke2-server"
  systemctl enable rke2-server.service
  systemctl start rke2-server.service
fi
{{- range .PostCommands }}
{{ . }}
{{- end }}

export KUBECONFIG={{ .Kubeconfig }}
export PATH="{{ .BinDir }}:$PATH"
log "waiting for the rke2 API server"
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
		"Kubeconfig":   shellQuote(RKE2Kubeconfig),
		"BinDir":       "/var/lib/rancher/rke2/bin",
	})
	if err != nil {
		panic(err)
	}
	return b.Bytes()
}

var postInitTemplate = template.Must(template.New("post-init").Parse(`#!/bin/bash
# Runs once the rke2 API server answers. It makes sure node zero is a labelled,
# Ready control plane, installs cert-manager and the Cluster API operator, and
# seeds the cluster Secrets RKE2ControlPlane looks up. rke2 ships its own CNI
# (canal), so there is no CNI step. Every step is safe to repeat.
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

# rke2 already labels its server nodes node-role.kubernetes.io/control-plane;
# make sure, so the operator's provider deployments (which select control plane
# nodes) schedule and the ordered self-managed deletion works.
log "labelling node zero as a control plane"
retry 60 5 bash -c 'n="$(kubectl get nodes -o jsonpath={.items[0].metadata.name})"; [ -n "$n" ] && kubectl label node "$n" node-role.kubernetes.io/control-plane= --overwrite'
{{- if .ContainerNodes }}

# Set node zero's providerID so Cluster API links the adopted Machine to its
# node. CAPD sets the DockerMachine providerID to docker:////<container>, whose
# name equals node zero's hostname; rke2's kubelet does not set a providerID on
# its own. Without this the Machine never gets a status.nodeRef.
log "setting node zero providerID for CAPD"
retry 60 5 bash -c 'n="$(kubectl get nodes -o jsonpath={.items[0].metadata.name})"; [ -n "$n" ] && kubectl patch node "$n" --type merge -p "{\"spec\":{\"providerID\":\"docker:////$n\"}}"'
{{- end }}

retry 60 10 kubectl wait --for=condition=Ready nodes --all --timeout=10s

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
		"Kubeconfig":     shellQuote(RKE2Kubeconfig),
		"BinDir":         "/var/lib/rancher/rke2/bin",
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
