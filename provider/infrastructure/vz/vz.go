// Package vz is the infrastructure provider for clusters managed by the Cluster
// API vz provider (klotho-k8s/cluster-api-provider-vz), Apple's
// Virtualization.framework on an Apple-Silicon Mac. It boots the bootstrap node
// as a single vz VM through the host-side capvz-daemon, validates (but never
// allocates) the control plane endpoint, renders the VZMachine that adopts the
// running VM, and tears everything down by name and the cluster tag.
//
// # klotho reaches the daemon over gRPC
//
// Virtualization.framework is cgo-bound, main-thread affine and has no remote
// API, so the provider repo splits into a Linux controller, a host-side
// capvz-daemon (a launchd LaunchAgent on the Mac), and one capvz-runner process
// per VM. klotho the CLI never links Virtualization.framework: it drives the
// bootstrap VM by calling the same capvz-daemon the in-cluster controller calls,
// over gRPC with TLS and a bearer token. This adapter is pure Go and builds
// anywhere; its gRPC client is fakeable, so its logic is unit-tested without a
// Mac. Because the daemon is reachable from outside the cluster, no deletion
// sidecar is needed: klotho's own Teardown finishes the last node from the host,
// so the fire-and-forget delete simply returns.
//
// # The daemon builds the cidata seed
//
// Unlike the Hyper-V and libvirt adapters, klotho does not build the NoCloud
// cidata seed itself here. The capvz-daemon builds it on the Mac from the raw
// user-data klotho sends over AttachCloudInit (Virtualization.framework has no
// CD/DVD device, so the seed is attached as a second read-only virtio-block
// device the NoCloud datasource finds by its CIDATA label). The daemon seeds the
// meta-data instance-id with the vmid it assigned, so the kubelet --provider-id
// "vz://<host>/{{ ds.meta_data.instance_id }}" resolves to the same ID the
// adopted VZMachine carries and the Machine<->Node nodeRef completes (there is no
// cloud controller manager). klotho's job is to deliver the bootstrap user-data
// to the daemon verbatim.
//
// # Name conventions (the contract Adopt, Plan and Teardown rely on)
//
// Nothing is recorded by vmid in state as the lookup key; these conventions are
// the contract, and they match the Cluster API vz provider's vmservice so it
// adopts what klotho creates:
//
//   - the bootstrap VM is named VMName(cluster), "<cluster>-bootstrap", which
//     klotho also sets as the adopted VZMachine's spec.vmName (the provider looks
//     a VM up by spec.vmName and adopts an existing one instead of creating it);
//   - every VM this provider creates carries the cluster tag Tag(cluster),
//     "klotho-<cluster>", in its notes marker, so Plan and Teardown can tell
//     klotho's VMs from others on the same host;
//   - the vmid is not stored: Adopt and Plan look it up again by name.
//
// # Provider ID and the endpoint
//
// A machine's provider ID is "vz://<host>/<vmid>", where <host> is the vz host
// (Config.Host or the VZ_HOST variable, default "localhost") and <vmid> is the
// UUID the daemon assigns the VM. vz has no load balancer, so the control plane
// endpoint is the single control plane node's own address. klotho does not
// allocate it: the user sets CONTROL_PLANE_ENDPOINT_HOST (or vz.endpointHost) to
// the address node zero will have on the NAT network, and Prepare only validates
// it. This is the single-node "endpoint is the bootstrap node" model the
// provider's flat cluster template is built for.
package vz

import (
	"fmt"
	"strings"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as used by --provider, in state,
// and as the key into config.providers.
const ProviderName = "vz"

// TagPrefix starts the notes marker every VM of a klotho cluster carries.
const TagPrefix = "klotho-"

// ProviderIDPrefix is the vz provider-ID scheme; it matches the Cluster API vz
// provider's provider-ID prefix so the provider IDs agree. A full provider ID is
// "vz://<host>/<vmid>".
const ProviderIDPrefix = "vz://"

// DefaultHost is the vz host recorded in provider IDs when none is configured.
// The single-host dev layout is one Apple-Silicon Mac, usually "localhost".
const DefaultHost = "localhost"

// Defaults applied when a Config field and the matching template variable are
// both unset.
const (
	// DefaultPort is the control plane port.
	DefaultPort = 6443
	// DefaultMemoryMiB and DefaultCPUs size the bootstrap VM when neither the
	// config nor the template set them. They match the vz provider's VZMachine
	// kubebuilder defaults closely enough for a dev control-plane node.
	DefaultMemoryMiB = 4096
	DefaultCPUs      = 2
)

// Template variables, matching the names the Cluster API vz provider's cluster
// template uses.
const (
	VarEndpointHost = "CONTROL_PLANE_ENDPOINT_HOST"
	VarHost         = "VZ_HOST"
	VarImagePath    = "VZ_IMAGE_PATH"
)

var _ infrastructure.Provider = (*Provider)(nil)

// Config configures the provider. A zero field falls back to the cluster's
// template variables and then to a default.
type Config struct {
	// Endpoint is the capvz-daemon's gRPC address klotho dials, for example
	// "my-mac.local:7443" or "192.168.5.20:7443". Required for any operation
	// that reaches the daemon (Launch, Adopt, Plan, Teardown, Discover).
	Endpoint string
	// CA is the path to a PEM CA bundle that signs the daemon's TLS certificate.
	// Empty means the host's system root CAs. For the daemon's first-run
	// self-signed certificate, point this at the cert the daemon wrote to its
	// data dir.
	CA string
	// ServerName overrides the TLS server name verified against the daemon's
	// certificate. Empty means the Endpoint host is used. Set it when dialing by
	// IP a certificate issued for a hostname.
	ServerName string
	// Token is the bearer token every gRPC call presents. Empty means the
	// daemon rejects the call; it comes from the environment (KLOTHO_VZ_TOKEN)
	// too and is handed to the in-cluster controller as VZ_DAEMON_TOKEN.
	Token string
	// Host is the vz host recorded in provider IDs (vz://<host>/<vmid>). Empty
	// means the VZ_HOST variable, then DefaultHost. It is informational for the
	// single-host dev layout, not a second dial target.
	Host string
	// Image is the path, on the Mac host, to the raw arm64 cloud-init enabled
	// base disk image the daemon copies for the bootstrap VM. Empty means the
	// VZ_IMAGE_PATH variable. Required, since a VM cannot boot without a disk;
	// the provider never downloads or builds it.
	Image string
	// MemoryMiB and CPUs size the bootstrap VM. Zero means DefaultMemoryMiB and
	// DefaultCPUs.
	MemoryMiB int
	CPUs      int
	// EndpointHost is the control plane endpoint address (a static address,
	// since vz has no load balancer). Empty means the CONTROL_PLANE_ENDPOINT_HOST
	// variable; Prepare fails if neither is set.
	EndpointHost string
}

// Provider implements infrastructure.Provider for Apple's Virtualization.framework.
type Provider struct {
	client Client
	cfg    Config
}

// New returns a provider that drives the capvz-daemon through client. When
// client is nil a gRPC-backed client is built lazily from cfg (its connection,
// TLS and token are set up on the first call), so building the provider and
// read-only commands that never reach the daemon do not require a reachable Mac.
func New(client Client, cfg Config) *Provider {
	if client == nil {
		client = newLazyClient(cfg)
	}
	return &Provider{client: client, cfg: cfg}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns the notes marker every VM of cluster carries.
func Tag(cluster string) string { return TagPrefix + strings.ToLower(cluster) }

// VMName returns the bootstrap VM's name, which is also the adopted VZMachine's
// spec.vmName.
func VMName(cluster string) string { return cluster + "-bootstrap" }

// ProviderID returns the full provider ID for a vmid on host.
func ProviderID(host, vmid string) string {
	return ProviderIDPrefix + host + "/" + vmid
}

// ParseProviderID splits a "vz://<host>/<vmid>" provider ID into its host and
// vmid. It reports ok=false for any value that is not a vz provider ID with both
// parts present.
func ParseProviderID(id string) (host, vmid string, ok bool) {
	rest, found := strings.CutPrefix(id, ProviderIDPrefix)
	if !found {
		return "", "", false
	}
	host, vmid, found = strings.Cut(rest, "/")
	if !found || host == "" || vmid == "" {
		return "", "", false
	}
	return host, vmid, true
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("vz provider: "+format, args...)
}

// resolved gathers the config-or-variable-or-default values a run needs.
type resolved struct {
	Host         string
	Image        string
	MemoryMiB    int
	CPUs         int
	EndpointHost string
}

// resolve fills a resolved from the config, the spec's variables, and defaults,
// in that order of precedence. It makes no API calls.
func (p *Provider) resolve(vars map[string]string) resolved {
	get := func(cfg, varName string) string {
		if cfg != "" {
			return cfg
		}
		return strings.TrimSpace(vars[varName])
	}
	r := resolved{
		Host:         get(p.cfg.Host, VarHost),
		Image:        get(p.cfg.Image, VarImagePath),
		MemoryMiB:    p.cfg.MemoryMiB,
		CPUs:         p.cfg.CPUs,
		EndpointHost: get(p.cfg.EndpointHost, VarEndpointHost),
	}
	if r.Host == "" {
		r.Host = DefaultHost
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = DefaultMemoryMiB
	}
	if r.CPUs == 0 {
		r.CPUs = DefaultCPUs
	}
	return r
}
