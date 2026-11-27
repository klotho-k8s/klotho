// Package libvirt is the infrastructure provider for clusters managed by the
// Cluster API libvirt provider. It boots the bootstrap node as a single
// libvirt/KVM domain from a qcow2 overlay of a base cloud image with a NoCloud
// cidata seed attached, validates (but never allocates) the control plane
// endpoint, renders the LibvirtMachine that adopts the running domain, and
// tears everything down by domain name and the cluster tag in the domain's
// description.
//
// # klotho reaches libvirtd directly
//
// libvirtd exposes a remote API, so klotho the CLI drives the bootstrap domain
// by connecting straight to it over the connection URI (a local socket
// "qemu:///system", or "qemu+tcp://host/system" / "qemu+tls://host/system" for
// a remote host). There is no custom host agent and, because libvirtd is
// reachable from outside the cluster, no deletion sidecar is needed: klotho's
// own Teardown finishes the last node from the host, so the fire-and-forget
// delete simply returns. The in-cluster Cluster API libvirt controller reaches
// the same libvirtd over the same URI to reconcile any further machines.
//
// # Name conventions (the contract Adopt, Plan and Teardown rely on)
//
// Nothing is recorded by domain UUID in state as the lookup key; these
// conventions are the contract, and they match the Cluster API libvirt
// provider's vmservice so it adopts what klotho creates:
//
//   - the bootstrap domain is named DomainName(cluster), "<cluster>-bootstrap",
//     which klotho also sets as the adopted LibvirtMachine's spec.domainName
//     (the provider looks a domain up by spec.domainName and adopts an existing
//     one instead of defining a new one);
//   - every domain this provider creates carries the cluster tag Tag(cluster),
//     "klotho-<cluster>", in its <description>, so Plan and Teardown can tell
//     klotho's domains from others on the same host;
//   - the bootstrap domain's NoCloud seed and overlay are OverlayName(cluster)
//     and CIDataName(cluster) inside the storage pool;
//   - the domain UUID is not stored: Adopt and Plan look it up again by name.
//
// # Provider ID and the endpoint
//
// A machine's provider ID is "libvirt://<host>/<domain-uuid>", where <host> is
// the host of the connection URI (or "localhost" for a local socket). klotho
// seeds the NoCloud meta-data instance-id with the domain UUID it assigns, so
// the kubelet --provider-id "libvirt://<host>/{{ ds.meta_data.instance_id }}"
// resolves to the same ID the adopted LibvirtMachine carries and the
// Machine<->Node nodeRef completes (there is no cloud controller manager).
//
// libvirt has no load balancer, so the control plane endpoint is the single
// control plane node's own address. klotho does not allocate it: the user sets
// CONTROL_PLANE_ENDPOINT_HOST (or libvirt.endpointHost) to the address node
// zero will have on the NAT network (a static address, or a DHCP reservation),
// and Prepare only validates it. This is the single-node "endpoint is the
// bootstrap node" model the provider's flat cluster template is built for.
package libvirt

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as used by --provider, in state,
// and as the key into config.providers.
const ProviderName = "libvirt"

// TagPrefix starts the description tag every domain of a klotho cluster carries.
const TagPrefix = "klotho-"

// ProviderIDPrefix is the libvirt provider-ID scheme; it matches the Cluster
// API libvirt provider's infrav1.ProviderIDPrefix so the provider IDs agree.
// A full provider ID is "libvirt://<host>/<domain-uuid>".
const ProviderIDPrefix = "libvirt://"

// Defaults applied when a Config field and the matching template variable are
// both unset.
const (
	// DefaultPort is the control plane port.
	DefaultPort = 6443
	// DefaultStoragePool is the libvirt storage pool the overlay and cidata
	// seed are created in, and where the base image is looked up.
	DefaultStoragePool = "default"
	// DefaultNetwork is the libvirt virtual network the NIC attaches to. The
	// stock "default" network is a NAT network that hands out DHCP leases and
	// reaches the internet, which suits a dev cluster.
	DefaultNetwork = "default"
	// DefaultNetworkType is how the NIC attaches: a libvirt virtual network.
	DefaultNetworkType = "network"
	// DefaultMemoryMiB and DefaultCPUs size the bootstrap domain when neither
	// the config nor the template set them.
	DefaultMemoryMiB = 4096
	DefaultCPUs      = 2
)

// Template variables, matching the names the Cluster API libvirt provider's
// cluster template uses.
const (
	VarEndpointHost = "CONTROL_PLANE_ENDPOINT_HOST"
	VarLibvirtHost  = "LIBVIRT_HOST"
	VarURI          = "LIBVIRT_URI"
	VarImage        = "LIBVIRT_IMAGE"
	VarPool         = "LIBVIRT_POOL"
	VarNetwork      = "LIBVIRT_NETWORK"
)

var _ infrastructure.Provider = (*Provider)(nil)

// Config configures the provider. A zero field falls back to the cluster's
// template variables and then to a default.
type Config struct {
	// URI is the libvirt connection URI klotho the CLI dials, for example
	// "qemu:///system" (a local socket), "qemu+tcp://host/system" or
	// "qemu+tls://host/system". Empty means "qemu:///system".
	URI string
	// ControllerURI is the connection URI the in-cluster Cluster API libvirt
	// controller uses to reach the same libvirtd. It MUST be a network URI the
	// controller pods can reach (qemu+tcp:// or qemu+tls://): a local-socket URI
	// such as "qemu:///system" is unreachable from a pod. Empty falls back to
	// URI, which then must itself be a network URI. The host part of this URI is
	// what provider IDs ("libvirt://<host>/<uuid>") are built from, since it is
	// what the controller derives when it sets each LibvirtMachine's providerID
	// and what the kubelet registers as LIBVIRT_HOST - so klotho the CLI can
	// dial a local socket (URI) while the controller, the kubelet and klotho's
	// own provider IDs all agree on the network host of ControllerURI.
	ControllerURI string
	// StoragePool holds the base image and receives the per-domain overlay and
	// the cidata seed. Empty means the LIBVIRT_POOL variable, then
	// DefaultStoragePool.
	StoragePool string
	// Image is the name of the base cloud image volume inside StoragePool the
	// overlay is backed by. Empty means the LIBVIRT_IMAGE variable. Required,
	// since a domain cannot boot without a disk; the provider never downloads
	// or builds it.
	Image string
	// Network is the libvirt virtual network (NetworkType "network") or host
	// bridge (NetworkType "bridge") the NIC attaches to. Empty means the
	// LIBVIRT_NETWORK variable, then DefaultNetwork.
	Network string
	// NetworkType is "network" (a libvirt virtual network, the default) or
	// "bridge" (a host bridge). Empty means DefaultNetworkType.
	NetworkType string
	// MemoryMiB and CPUs size the bootstrap domain. Zero means DefaultMemoryMiB
	// and DefaultCPUs.
	MemoryMiB int
	CPUs      int
	// DiskGiB optionally grows the overlay's virtual capacity. Zero inherits
	// the base image's virtual size.
	DiskGiB int
	// EndpointHost is the control plane endpoint address (a static address,
	// since libvirt has no load balancer). Empty means the
	// CONTROL_PLANE_ENDPOINT_HOST variable; Prepare fails if neither is set.
	EndpointHost string
}

// Provider implements infrastructure.Provider for libvirt/KVM.
type Provider struct {
	client Client
	cfg    Config
}

// New returns a provider that drives libvirtd through client. When client is
// nil a go-libvirt-backed client is built from cfg's connection URI
// (NewClient); its connection is opened lazily on the first call.
func New(client Client, cfg Config) *Provider {
	if client == nil {
		client = newLazyClient(resolveURI(cfg))
	}
	return &Provider{client: client, cfg: cfg}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns the description tag every domain of cluster carries.
func Tag(cluster string) string { return TagPrefix + strings.ToLower(cluster) }

// DomainName returns the bootstrap domain's name, which is also the adopted
// LibvirtMachine's spec.domainName.
func DomainName(cluster string) string { return cluster + "-bootstrap" }

// OverlayName returns the name of the bootstrap domain's qcow2 overlay volume.
func OverlayName(cluster string) string { return cluster + "-bootstrap.qcow2" }

// CIDataName returns the name of the bootstrap domain's NoCloud cidata volume.
func CIDataName(cluster string) string { return cluster + "-bootstrap-cidata.iso" }

// ProviderID returns the full provider ID for a domain UUID on host.
func ProviderID(host, uuid string) string {
	return ProviderIDPrefix + host + "/" + uuid
}

// HostFromURI returns the host recorded in provider IDs for a connection URI:
// its hostname, or "localhost" for a local socket URI with no host.
func HostFromURI(uri string) string {
	if u, err := url.Parse(uri); err == nil {
		if h := u.Hostname(); h != "" {
			return h
		}
	}
	return "localhost"
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("libvirt provider: "+format, args...)
}

// resolved gathers the config-or-variable-or-default values a run needs.
type resolved struct {
	URI           string
	ControllerURI string
	Host          string
	StoragePool   string
	Image         string
	Network       string
	NetworkType   string
	MemoryMiB     int
	CPUs          int
	DiskGiB       int
	EndpointHost  string
}

// resolve fills a resolved from the config, the spec's variables, and
// defaults, in that order of precedence. It makes no API calls.
func (p *Provider) resolve(vars map[string]string) resolved {
	get := func(cfg, varName string) string {
		if cfg != "" {
			return cfg
		}
		return strings.TrimSpace(vars[varName])
	}
	r := resolved{
		URI:           resolveURI(p.cfg),
		ControllerURI: ControllerURI(p.cfg),
		StoragePool:   get(p.cfg.StoragePool, VarPool),
		Image:         get(p.cfg.Image, VarImage),
		Network:       get(p.cfg.Network, VarNetwork),
		NetworkType:   p.cfg.NetworkType,
		MemoryMiB:     p.cfg.MemoryMiB,
		CPUs:          p.cfg.CPUs,
		DiskGiB:       p.cfg.DiskGiB,
		EndpointHost:  get(p.cfg.EndpointHost, VarEndpointHost),
	}
	r.Host = HostFromURI(r.ControllerURI)
	if r.StoragePool == "" {
		r.StoragePool = DefaultStoragePool
	}
	if r.Network == "" {
		r.Network = DefaultNetwork
	}
	if r.NetworkType == "" {
		r.NetworkType = DefaultNetworkType
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = DefaultMemoryMiB
	}
	if r.CPUs == 0 {
		r.CPUs = DefaultCPUs
	}
	return r
}

// resolveURI returns the connection URI klotho the CLI dials from config, else
// the local system socket.
func resolveURI(cfg Config) string {
	if cfg.URI != "" {
		return cfg.URI
	}
	return "qemu:///system"
}

// ControllerURI returns the connection URI the in-cluster Cluster API libvirt
// controller uses: cfg.ControllerURI when set, else the host dial URI (which
// then must itself be reachable from the controller pods). klotho the CLI dials
// resolveURI(cfg); only the controller - and provider IDs - use this one.
func ControllerURI(cfg Config) string {
	if u := strings.TrimSpace(cfg.ControllerURI); u != "" {
		return u
	}
	return resolveURI(cfg)
}

// idHost is the host recorded in provider IDs: the host of the controller
// connection URI. It must match the host the in-cluster controller derives from
// that URI when it sets each LibvirtMachine's providerID, and the LIBVIRT_HOST
// the kubelet registers with, so the Machine<->Node nodeRef completes.
func (p *Provider) idHost() string { return HostFromURI(ControllerURI(p.cfg)) }

// localSocketURI reports whether uri has no network host (a local socket such
// as qemu:///system or qemu+unix://...), which the in-cluster controller cannot
// reach from a pod.
func localSocketURI(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	return u.Hostname() == ""
}
