package kubeadm

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/template"

	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Jinja expressions the bootstrap node can resolve, mapped to the shell
// variable that holds their value at boot. Each is read with
// "cloud-init query <expression>".
var knownJinja = map[string]string{
	"ds.meta_data.instance_id":    "INSTANCE_ID",
	"ds.meta_data.hostname":       "HOSTNAME",
	"ds.meta_data.local_hostname": "LOCAL_HOSTNAME",
}

const jinjaTokenPrefix = "@@KLOTHO_JINJA_"

var jinjaExpr = regexp.MustCompile(`\{\{-?\s*([^{}]*?)\s*-?\}\}`)

func hasJinja(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, "{%") || strings.Contains(s, "{#")
}

// jinjaRenderer replaces known Jinja expressions with tokens at render time
// and produces the boot-time script that fills the tokens in.
type jinjaRenderer struct {
	files []string
	exprs map[string]bool
}

func newJinjaRenderer() *jinjaRenderer { return &jinjaRenderer{exprs: map[string]bool{}} }

func (j *jinjaRenderer) needed() bool { return len(j.files) > 0 }

// prepare returns content with every known Jinja expression replaced by its
// token, recording path for the boot-time rewrite. Unknown expressions,
// statements and comments are an error.
func (j *jinjaRenderer) prepare(path string, content []byte) ([]byte, error) {
	s := string(content)
	if !hasJinja(s) {
		return content, nil
	}
	if strings.Contains(s, "{%") || strings.Contains(s, "{#") {
		return nil, fmt.Errorf("kubeadm: %s: Jinja statements and comments are not supported", path)
	}
	var unknown []string
	out := jinjaExpr.ReplaceAllStringFunc(s, func(m string) string {
		expr := jinjaExpr.FindStringSubmatch(m)[1]
		name, ok := knownJinja[expr]
		if !ok {
			unknown = append(unknown, m)
			return m
		}
		j.exprs[expr] = true
		return jinjaTokenPrefix + name + "@@"
	})
	if len(unknown) > 0 {
		return nil, fmt.Errorf("kubeadm: %s: unsupported Jinja expressions %s (supported: %s)",
			path, strings.Join(unknown, ", "), strings.Join(knownJinjaNames(), ", "))
	}
	if strings.Contains(out, "{{") {
		return nil, fmt.Errorf("kubeadm: %s: malformed Jinja expression", path)
	}
	j.files = append(j.files, path)
	return []byte(out), nil
}

func knownJinjaNames() []string {
	names := make([]string, 0, len(knownJinja))
	for k := range knownJinja {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (j *jinjaRenderer) script() []byte {
	var b strings.Builder
	b.WriteString(`#!/bin/bash
# Resolves the cloud-init Jinja expressions the kubeadm bootstrap provider
# would have had cloud-init render, then marks the node ready for kubeadm.
set -euo pipefail

fail() { echo "render-jinja: $*" >&2; exit 1; }

value() {
  local v
  v="$(cloud-init query "$1" 2>/dev/null)" || fail "cloud-init query $1 failed"
  [ -n "$v" ] || fail "$1 is empty"
  printf '%s' "$v"
}

files=(`)
	for _, f := range j.files {
		b.WriteString(" " + shellQuote(f))
	}
	b.WriteString(")\n")
	for _, expr := range knownJinjaNames() {
		if !j.exprs[expr] {
			continue
		}
		v := "JINJA_" + knownJinja[expr]
		fmt.Fprintf(&b, "\n%s=\"$(value %s)\"\n", v, expr)
		fmt.Fprintf(&b, "esc=\"$(printf '%%s' \"$%s\" | sed -e 's/[\\\\|&]/\\\\&/g')\"\n", v)
		fmt.Fprintf(&b, "sed -i \"s|%s%s@@|${esc}|g\" \"${files[@]}\"\n", jinjaTokenPrefix, knownJinja[expr])
	}
	fmt.Fprintf(&b, `
if grep -l '%s' "${files[@]}"; then
  fail "unresolved expressions remain"
fi
touch %s
`, jinjaTokenPrefix, JinjaDoneMarker)
	return []byte(b.String())
}

// decodeFileContent returns a spec file's bytes as they land on disk, and
// whether the content was plain text (and so subject to Jinja rendering in
// the kubeadm bootstrap provider's user-data).
func decodeFileContent(f bootstrapv1.File) ([]byte, bool, error) {
	raw := []byte(f.Content)
	switch f.Encoding {
	case "":
		return raw, true, nil
	case bootstrapv1.Base64:
		out, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return nil, false, fmt.Errorf("kubeadm: file %s: %w", f.Path, err)
		}
		return out, false, nil
	case bootstrapv1.Gzip:
		out, err := gunzip(raw)
		if err != nil {
			return nil, false, fmt.Errorf("kubeadm: file %s: %w", f.Path, err)
		}
		return out, false, nil
	case bootstrapv1.GzipBase64:
		z, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return nil, false, fmt.Errorf("kubeadm: file %s: %w", f.Path, err)
		}
		out, err := gunzip(z)
		if err != nil {
			return nil, false, fmt.Errorf("kubeadm: file %s: %w", f.Path, err)
		}
		return out, false, nil
	}
	return nil, false, fmt.Errorf("kubeadm: file %s: unknown encoding %q", f.Path, f.Encoding)
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// usersScript creates the spec's non-root users the way cloud-init's users
// module would, and installs their SSH keys.
func usersScript(users []bootstrapv1.User) []byte {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n")
	for _, u := range users {
		name := shellQuote(u.Name)
		args := []string{"-m"}
		if u.HomeDir != "" {
			args = append(args, "-d", shellQuote(u.HomeDir))
		}
		if u.Shell != "" {
			args = append(args, "-s", shellQuote(u.Shell))
		}
		if u.Gecos != "" {
			args = append(args, "-c", shellQuote(u.Gecos))
		}
		if u.PrimaryGroup != "" {
			args = append(args, "-g", shellQuote(u.PrimaryGroup))
		}
		fmt.Fprintf(&b, "\nid -u %s >/dev/null 2>&1 || useradd %s %s\n", name, strings.Join(args, " "), name)
		if g := strings.ReplaceAll(u.Groups, " ", ""); g != "" {
			fmt.Fprintf(&b, "usermod -aG %s %s\n", shellQuote(g), name)
		}
		if u.Sudo != "" {
			sudoers := shellQuote("/etc/sudoers.d/90-klotho-" + u.Name)
			fmt.Fprintf(&b, "printf '%%s %%s\\n' %s %s > %s\nchmod 0440 %s\n", name, shellQuote(u.Sudo), sudoers, sudoers)
		}
		if len(u.SSHAuthorizedKeys) > 0 {
			fmt.Fprintf(&b, "home=\"$(getent passwd %s | cut -d: -f6)\"\n", name)
			fmt.Fprintf(&b, "install -d -m 0700 -o %s -g \"$(id -gn %s)\" \"$home/.ssh\"\n", name, name)
			b.WriteString("touch \"$home/.ssh/authorized_keys\"\n")
			for _, k := range u.SSHAuthorizedKeys {
				q := shellQuote(k)
				fmt.Fprintf(&b, "grep -qxF %s \"$home/.ssh/authorized_keys\" || printf '%%s\\n' %s >> \"$home/.ssh/authorized_keys\"\n", q, q)
			}
			fmt.Fprintf(&b, "chown %s:\"$(id -gn %s)\" \"$home/.ssh/authorized_keys\"\nchmod 0600 \"$home/.ssh/authorized_keys\"\n", name, name)
		}
	}
	return []byte(b.String())
}

// endpointFixScript runs before kubeadm init. kubeadm init's post-control-plane
// phases (addons: kube-proxy and CoreDNS; the admin and kubelet-api RBAC) reach
// the API server through the admin kubeconfig, whose server is
// controlPlaneEndpoint. When that endpoint is not an address this node holds --
// a DHCP-addressed node (e.g. Hyper-V) whose endpoint could only be a
// placeholder before boot -- those phases fail silently and leave the cluster
// without kube-proxy, CoreDNS and the cluster-admin RBAC, which breaks the CNI
// and every later step. Pointing controlPlaneEndpoint at this node's primary
// IPv4, which kubeadm also includes in the API server certificate SANs, makes
// every init phase reach the local API server so the cluster comes up complete.
// A control-plane endpoint that is already an address on this node (a static
// address or a claimed VIP) is left unchanged.
const endpointFixScript = `#!/bin/bash
set -euo pipefail
cfg=/run/kubeadm/kubeadm.yaml
[ -f "$cfg" ] || exit 0
line="$(grep -E '^[[:space:]]*controlPlaneEndpoint:' "$cfg" | head -1 || true)"
[ -n "$line" ] || exit 0
value="$(printf '%s' "$line" | sed -E 's#^[[:space:]]*controlPlaneEndpoint:[[:space:]]*##; s#^"##; s#"$##')"
host="$(printf '%s' "$value" | sed -E 's#:[0-9]+$##')"
port="$(printf '%s' "$value" | sed -nE 's#.*:([0-9]+)$#\1#p')"
[ -n "$port" ] || port=6443
if [ -n "$host" ] && ! ip -4 -o addr show 2>/dev/null | grep -Fqw "$host"; then
  node_ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "src") {print $(i + 1); exit}}')"
  if [ -n "$node_ip" ]; then
    sed -i -E "s#^([[:space:]]*controlPlaneEndpoint:[[:space:]]*).*#\1${node_ip}:${port}#" "$cfg"
    echo "endpoint-fix: controlPlaneEndpoint ${host} is not an address on this node; set to ${node_ip}:${port}" >&2
  fi
fi
`

var postInitTemplate = template.Must(template.New("post-init").Parse(`#!/bin/bash
# Runs once kubeadm init has returned. Every step is safe to repeat and waits
# for what the next step needs.
set -euo pipefail

export KUBECONFIG={{ .Kubeconfig }}
SERVER={{ .Server }}

# kubeadm 1.29+ binds admin.conf to the kubeadm:cluster-admins group through a
# ClusterRoleBinding created by the init RBAC phase; when that phase has not run
# the admin user is unprivileged ("User kubernetes-admin cannot ... Forbidden").
# super-admin.conf is bound directly to system:masters and always has full
# access, so prefer it for the cluster-admin steps below (applying the CNI,
# cert-manager, the operator and the seed manifests).
if [ -f /etc/kubernetes/super-admin.conf ]; then
  export KUBECONFIG=/etc/kubernetes/super-admin.conf
fi

log() { echo "post-init: $*" >&2; }

# post-init runs on the freshly kubeadm-init'd control-plane node, so its API
# server is always reachable locally. The rendered endpoint may be an address
# this node does not hold: on a DHCP-addressed Hyper-V node the control-plane
# endpoint could only be a placeholder before boot. When the endpoint is not an
# address on this node, target this node's primary IPv4 instead, which kubeadm
# includes in the API server certificate SANs, and rewrite the admin kubeconfig
# server to match so every kubectl below (which reads KUBECONFIG, not SERVER)
# reaches the local API server, not the unreachable placeholder.
endpoint_host="$(printf '%s' "$SERVER" | sed -E 's#^[a-z]+://##; s#:[0-9]+$##')"
if [ -z "$endpoint_host" ] || ! ip -4 -o addr show 2>/dev/null | grep -Fqw "$endpoint_host"; then
  node_ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "src") {print $(i + 1); exit}}')"
  if [ -n "$node_ip" ]; then
    log "control-plane endpoint ${endpoint_host:-unset} is not an address on this node; using this node's primary IPv4 $node_ip"
    SERVER="https://${node_ip}:6443"
    if [ -f "$KUBECONFIG" ]; then
      sed -i -E "s#^(\s*server: ).*#\1${SERVER}#" "$KUBECONFIG" || true
    fi
  fi
fi

# retry ATTEMPTS DELAY COMMAND...
retry() {
  local attempts=$1 delay=$2 i
  shift 2
  for ((i = 1; i <= attempts; i++)); do
    if "$@"; then
      return 0
    fi
    log "attempt $i/$attempts failed: $*"
    sleep "$delay"
  done
  log "giving up: $*"
  return 1
}

api_ready() { kubectl --server "$SERVER" --request-timeout=5s get --raw=/readyz >/dev/null; }

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

log "waiting for the API server at $SERVER"
retry 120 5 api_ready

log "applying the CNI"
# Server-side apply: the CNI and cert-manager manifests carry CRDs whose schema
# exceeds the 262144-byte limit kubectl's client-side last-applied annotation
# imposes, so a plain "kubectl apply -f" fails with "metadata.annotations: Too
# long" on those CRDs (e.g. Calico's IPPool), leaving the CNI unable to start.
retry 30 10 kubectl apply --server-side --force-conflicts -f {{ .CNI }}
retry 60 10 kubectl wait --for=condition=Ready nodes --all --timeout=10s

# With an external cloud provider (AWS's cloud-provider=external) the kubelet
# taints the node node.cloudprovider.kubernetes.io/uninitialized:NoSchedule
# until a cloud controller manager initializes it. On a single bootstrap node
# the CCM only arrives later with the cluster bundle, so that taint would block
# cert-manager, the operator and the whole management stack from scheduling.
# Remove it so bootstrap proceeds; the CCM still initializes this node and any
# workers once it runs. A no-op where the taint is absent (no external provider).
log "clearing the cloud-provider uninitialized taint for bootstrap"
kubectl taint nodes --all node.cloudprovider.kubernetes.io/uninitialized:NoSchedule- 2>/dev/null || true

log "applying cert-manager"
retry 30 10 kubectl apply --server-side --force-conflicts -f {{ .CertManager }}
# cert-manager's manifest tolerates no taints, and until a worker joins the
# only node is a tainted control plane. The node's taints stay as configured.
tolerate_control_plane() {
  local d
  for d in $(kubectl -n cert-manager get deployments -o name); do
    kubectl -n cert-manager patch "$d" --type strategic -p \
      '{"spec":{"template":{"spec":{"tolerations":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"},{"key":"node-role.kubernetes.io/master","operator":"Exists","effect":"NoSchedule"}]}}}}' >/dev/null
  done
}
retry 30 5 tolerate_control_plane
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

func (p *Provider) postInitScript(ep cluster.Endpoint) []byte {
	var b bytes.Buffer
	err := postInitTemplate.Execute(&b, map[string]any{
		"Kubeconfig":  shellQuote(AdminKubeconfig),
		"Server":      shellQuote(serverURL(ep)),
		"CNI":         shellQuote(p.opts.CNIManifestURL),
		"CertManager": shellQuote(p.opts.CertManagerURL),
		"Operator":    shellQuote(p.opts.OperatorURL),
		"Manifests":   shellQuote(ManifestsDir + "/"),
		"Extra":       p.opts.ExtraPostCommands,
	})
	if err != nil {
		// The template is static and its data is strings.
		panic(err)
	}
	return b.Bytes()
}

// shellQuote quotes s as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
