// Package proxmox is the infrastructure provider for clusters managed by the
// Cluster API Proxmox provider. It boots the bootstrap node by cloning a VM
// template and attaching a NoCloud ISO, validates (but never allocates) the
// control plane VIP, renders the ProxmoxMachine that adopts the running VM,
// and tears everything down by tag.
//
// # Name conventions
//
// Nothing this provider creates is recorded by ID in state; the following
// conventions are the contract Plan, Adopt and Teardown rely on:
//
//   - every VM it creates carries the tag Tag(cluster), "klotho-<cluster>",
//     and the bootstrap VM is named VMName(cluster), "<cluster>-bootstrap",
//     which is also the adopted ProxmoxMachine's name (the Cluster API
//     Proxmox provider only accepts an existing VM whose name equals the
//     machine's);
//   - the bootstrap VM's NoCloud ISO is ISOName(cluster),
//     "<cluster>-bootstrap-cidata.iso", in an ISO-capable storage;
//   - the Proxmox VMID is not stored: Adopt and Plan look it up again by
//     name and tag with one cluster resources call.
//
// Proxmox tags may only contain letters, digits and "_-+.", so the tag uses
// "-" rather than ":" as the separator.
package proxmox

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	gp "github.com/luthermonson/go-proxmox"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier.
const ProviderName = "proxmox"

// TagPrefix starts the tag every resource of a klotho cluster carries.
const TagPrefix = "klotho-"

// Defaults applied by Resolve.
const (
	DefaultBridge       = "vmbr0"
	DefaultAgentTimeout = 5 * time.Minute
	DefaultPort         = 6443
	// DefaultBootDevice is the boot disk grown when BOOT_VOLUME_SIZE is set
	// without BOOT_VOLUME_DEVICE.
	DefaultBootDevice = "scsi0"
)

// Template variables read from cluster.Spec.Variables. They are the names the
// Cluster API Proxmox provider's cluster templates use.
const (
	VarEndpointIP       = "CONTROL_PLANE_ENDPOINT_IP"
	VarEndpointPort     = "CONTROL_PLANE_ENDPOINT_PORT"
	VarAllowedNodes     = "ALLOWED_NODES"
	VarSourceNode       = "PROXMOX_SOURCENODE"
	VarTemplateID       = "TEMPLATE_VMID"
	VarBridge           = "BRIDGE"
	VarNumCores         = "NUM_CORES"
	VarNumSockets       = "NUM_SOCKETS"
	VarMemoryMiB        = "MEMORY_MIB"
	VarBootVolumeSize   = "BOOT_VOLUME_SIZE"
	VarBootVolumeDevice = "BOOT_VOLUME_DEVICE"
	// Network variables klotho statically addresses nodes from. The deletion
	// sidecar reuses them to address itself when no DHCP server is available.
	VarGateway      = "GATEWAY"
	VarIPPrefix     = "IP_PREFIX"
	VarDNSServers   = "DNS_SERVERS"
	VarNodeIPRanges = "NODE_IP_RANGES"
)

var _ infrastructure.Provider = (*Provider)(nil)

// Config configures the provider. Zero fields fall back to the cluster's
// template variables and then to defaults; see Resolve.
type Config struct {
	// URL, TokenID, Secret, Insecure and CACert configure the connection
	// NewClient opens.
	URL      string
	TokenID  string
	Secret   string
	Insecure bool
	CACert   []byte

	// Node is where the bootstrap VM runs. Empty means the first entry of
	// ALLOWED_NODES, then PROXMOX_SOURCENODE, then the template's node if it
	// is online, then the first online node.
	Node string
	// TemplateID is the VMID of the template to clone. Zero means
	// TemplateTag, then TEMPLATE_VMID.
	TemplateID int
	// TemplateTag selects the template by tag instead; exactly one template
	// may carry it.
	TemplateTag string
	// TemplateTags selects the template by tags with TemplateMatchPolicy,
	// the way a generated bundle's templateSelector does: the bootstrap node
	// is cloned from the template on its node that matches. With TemplateID
	// set as well, the two must agree. TemplateTags wins over TemplateTag
	// and TEMPLATE_VMID.
	TemplateTags []string
	// TemplateMatchPolicy is exact (default), uniqueSubset or bestSubset;
	// see the generate package's TemplateMatch constants.
	TemplateMatchPolicy string
	// ISOStorage receives the NoCloud ISO. Empty means the first active
	// storage on the node that allows iso content.
	ISOStorage string
	// Bridge is set on net0. Empty means BRIDGE; when neither is set net0 is
	// left as cloned (Resolved.Bridge reports DefaultBridge for display).
	Bridge string
	// Cores, Sockets and MemoryMiB size the VM. Zero means NUM_CORES,
	// NUM_SOCKETS and MEMORY_MIB, and otherwise the template's values.
	Cores     int
	Sockets   int
	MemoryMiB int
	// VMIDRange restricts the bootstrap VM's VMID to [start, end]. {0, 0}
	// means the cluster's next free ID.
	VMIDRange [2]int
	// FullClone makes a full copy of the template. Nil means true.
	FullClone *bool
	// Pool adds the VM to this existing resource pool. Empty means none.
	Pool string
	// Sidecar configures the token-only Alpine LXC that a default
	// (fire-and-forget) delete launches to finish a cluster's teardown after
	// klotho exits; see sidecar.go. The zero value is fully defaulted.
	Sidecar SidecarConfig
	// AgentTimeout bounds the wait for the guest agent to report an IPv4
	// address. Zero means DefaultAgentTimeout.
	AgentTimeout time.Duration
}

// SidecarConfig tunes the deletion sidecar. Every field has a safe default, so
// the zero value works; see the defaults in sidecar.go.
type SidecarConfig struct {
	// TemplateStorage receives the one-shot Alpine LXC template klotho builds
	// and the sidecar container's rootfs. Empty means the first active storage
	// on the node that allows, respectively, vztmpl and rootdir content.
	TemplateStorage string
	// RootfsStorage overrides the storage the sidecar's rootfs is allocated
	// on when it should differ from TemplateStorage. Empty means
	// TemplateStorage, then the first active rootdir-capable storage.
	RootfsStorage string
	// Bridge is the network bridge the sidecar's interface attaches to. Empty
	// means the provider's Bridge, then DefaultBridge.
	Bridge string
	// IP, Gateway and DNS statically address the sidecar. IP is an address with
	// an optional prefix ("192.168.1.221/20" or "192.168.1.221", prefix then
	// taken from IP_PREFIX). Empty IP derives a free address from the cluster's
	// NODE_IP_RANGES, GATEWAY, IP_PREFIX and DNS_SERVERS; when none of those is
	// available the sidecar falls back to DHCP. Gateway and DNS default to the
	// cluster's GATEWAY and the first DNS_SERVERS entry.
	IP      string
	Gateway string
	DNS     string
	// CurlURL, CurlSHA256, JqURL and JqSHA256 override the static curl and jq
	// binaries klotho bakes into the sidecar's rootfs so the sweep needs no
	// package install and no internet, only LAN reachability to the Proxmox
	// API. Empty means the pinned defaults; set a URL with its SHA-256.
	CurlURL    string
	CurlSHA256 string
	JqURL      string
	JqSHA256   string
	// AlpineRootfsURL is the Alpine minirootfs tarball klotho fetches, bakes
	// the sweep into, and uploads as a one-shot template. Empty means
	// DefaultAlpineRootfsURL.
	AlpineRootfsURL string
	// AlpineRootfsSHA256 is the hex SHA-256 the fetched rootfs must match.
	// Empty means DefaultAlpineRootfsSHA256 (paired with the default URL); set
	// both together when overriding the mirror or version.
	AlpineRootfsSHA256 string
	// TokenTTL bounds the scoped API token klotho mints for the sidecar. Zero
	// means DefaultSidecarTokenTTL. The token self-expires, so it is the
	// sidecar's credential lifetime even when nothing cleans it up.
	TokenTTL time.Duration
	// MemoryMiB sizes the sidecar container. Zero means DefaultSidecarMemoryMiB.
	MemoryMiB int
}

// Provider implements infrastructure.Provider for Proxmox VE.
type Provider struct {
	client *gp.Client
	cfg    Config
	// poll is the guest agent polling interval.
	poll time.Duration
	// taskPoll is how often a task's status is polled while waiting for it.
	taskPoll time.Duration
	// cleanupTimeout bounds best-effort cleanup after a failed Launch.
	cleanupTimeout time.Duration

	// seen is the cluster's template listing from the last Prepare or
	// Launch, so Check can review a bundle's template selectors against the
	// allowed nodes without touching the network. shared records, by storage
	// name, whether a storage is shared across nodes, and disks the storages
	// each inspected template's disks live on, both from the same listing;
	// together they tell whether a template can be cloned to another node.
	mu     sync.Mutex
	seen   []*gp.ClusterResource
	shared map[string]bool
	disks  map[int][]string
	// cloudInit lists, per inspected template, the Proxmox cloud-init
	// drives it carries (see cloudInitDrives).
	cloudInit map[int][]string

	// rootfsSource overrides where the deletion sidecar's base Alpine
	// minirootfs comes from; nil fetches the configured tarball. Tests set it
	// to supply a tiny in-memory rootfs.
	rootfsSource func(ctx context.Context) (io.ReadCloser, error)
	// toolSource overrides where the sidecar's baked curl and jq binaries come
	// from; nil fetches (and verifies) the configured URL. Tests set it to
	// supply tiny in-memory stand-ins without reaching the network.
	toolSource func(ctx context.Context, url, sha256 string) ([]byte, error)
}

// New returns a provider talking to Proxmox through client, normally built
// by NewClient.
func New(client *gp.Client, cfg Config) *Provider {
	return &Provider{
		client: client, cfg: cfg,
		poll: 5 * time.Second, taskPoll: DefaultTaskPollInterval, cleanupTimeout: 3 * time.Minute,
	}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns the tag every resource of cluster carries.
func Tag(cluster string) string { return TagPrefix + strings.ToLower(cluster) }

// VMName returns the bootstrap VM's name, which is also the adopted
// ProxmoxMachine's name.
func VMName(cluster string) string { return cluster + "-bootstrap" }

// ISOName returns the file name of the bootstrap VM's NoCloud ISO.
func ISOName(cluster string) string { return cluster + "-bootstrap-cidata.iso" }

// tagPattern is Proxmox's pve-tag format, which the Cluster API Proxmox
// provider's tags field enforces too.
var tagPattern = regexp.MustCompile(`^(?i)[a-z0-9_][a-z0-9_\-+.]*$`)

func hasKlothoTag(tags []string) bool {
	for _, t := range tags {
		if strings.HasPrefix(strings.ToLower(t), TagPrefix) {
			return true
		}
	}
	return false
}

func errorf(format string, args ...any) error {
	return fmt.Errorf("proxmox provider: "+format, args...)
}

// DefaultIPAMVersion is the in-cluster IPAM provider release installed with
// the Proxmox provider, which allocates node addresses through it.
const DefaultIPAMVersion = "v1.1.1"

// Dependencies implements provider.Dependent: the Proxmox provider cannot
// reconcile a ProxmoxCluster without the in-cluster IPAM provider's
// InClusterIPPool kind.
func (p *Provider) Dependencies() []provider.Dependency {
	return []provider.Dependency{{
		Kind: "IPAMProvider", Name: "in-cluster", Namespace: "capi-ipam-in-cluster-system", Version: DefaultIPAMVersion,
	}}
}

// RequiredPackages implements provider.PackageRequirer: both klotho and the
// Cluster API Proxmox provider learn a node's addresses from the QEMU guest
// agent, so a node without it never becomes ready.
func (p *Provider) RequiredPackages() []string { return []string{"qemu-guest-agent"} }
