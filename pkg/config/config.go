// Package config loads klotho's configuration file and environment
// overrides.
//
// The file lives at $XDG_CONFIG_HOME/klotho/config.yaml (falling back to
// ~/.config/klotho/config.yaml) unless a path is given. Every field is
// optional. A named profile is deep-merged over the top level, then
// environment variables are applied last:
//
//	KLOTHO_BACKEND              backend
//	KLOTHO_STATE_DIR            stateDir
//	KLOTHO_AGE_IDENTITY_FILE    encryption.identityFile
//	KLOTHO_AGE_RECIPIENTS       encryption.recipients (comma separated)
//	KLOTHO_PROXMOX_URL          proxmox.url
//	KLOTHO_PROXMOX_TOKEN_ID     proxmox.tokenId
//	KLOTHO_PROXMOX_SECRET       proxmox.secret
//	KLOTHO_PROXMOX_INSECURE     proxmox.insecure
//	KLOTHO_S3_ACCESS_KEY_ID     payload.s3 access key (environment only)
//	KLOTHO_S3_SECRET_ACCESS_KEY payload.s3 secret key (environment only)
//
// klotho never writes the Proxmox secret or the S3 keys back to disk, and
// the S3 keys are not accepted from the file at all.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/state"
)

// Environment variable names.
const (
	EnvBackend         = "KLOTHO_BACKEND"
	EnvStateDir        = "KLOTHO_STATE_DIR"
	EnvIdentityFile    = "KLOTHO_AGE_IDENTITY_FILE"
	EnvRecipients      = "KLOTHO_AGE_RECIPIENTS"
	EnvProxmoxURL      = "KLOTHO_PROXMOX_URL"
	EnvProxmoxTokenID  = "KLOTHO_PROXMOX_TOKEN_ID"
	EnvProxmoxSecret   = "KLOTHO_PROXMOX_SECRET"
	EnvProxmoxInsecure = "KLOTHO_PROXMOX_INSECURE"
	EnvS3AccessKeyID   = "KLOTHO_S3_ACCESS_KEY_ID"
	EnvS3SecretKey     = "KLOTHO_S3_SECRET_ACCESS_KEY"
	EnvLinodeToken     = "LINODE_TOKEN"
	// EnvGCPCredentials is the standard GCP service-account key path; klotho
	// reads it for gcp.credentialsFile when that is unset.
	EnvGCPCredentials = "GOOGLE_APPLICATION_CREDENTIALS"
	// EnvAzureClientSecret is the Azure service-principal client secret.
	EnvAzureClientSecret = "AZURE_CLIENT_SECRET"
)

// Defaults.
const (
	DefaultCAPIVersion    = "v1.12.9"
	DefaultProxmoxVersion = "v0.9.2"
	// DefaultAWSVersion is the Cluster API AWS (CAPA) release installed for
	// the aws provider. VERIFY against a live run; CAPA's current contract
	// may require a different release.
	DefaultAWSVersion = "v2.8.2"
	// DefaultLinodeVersion is the Cluster API Provider Linode (CAPL) release
	// installed for the linode provider. The v0.10 series is the first on the
	// Cluster API v1beta2 contract klotho requires. VERIFY against a live run.
	DefaultLinodeVersion = "v0.10.17"
	// DefaultGCPVersion is the Cluster API Provider GCP (CAPG) release installed
	// for the gcp provider. CAPG's infrastructure objects are v1beta1; the flat
	// cluster template authors the CAPI-core objects in the v1beta2 contract
	// klotho requires. VERIFY the release and the contract compatibility against
	// a live run.
	DefaultGCPVersion = "v1.8.1"
	// DefaultAzureVersion is the Cluster API Provider Azure (CAPZ) release
	// installed for the azure provider. CAPZ v1.23.x is built against Cluster
	// API v1.12.x (DefaultCAPIVersion), and its infrastructure CRDs are
	// infrastructure.cluster.x-k8s.io/v1beta1. VERIFY against a live run.
	DefaultAzureVersion = "v1.23.3"
	// ProviderAWS is the aws infrastructure provider's name, as clusterctl
	// and --provider know it.
	ProviderAWS = "aws"
	// ProviderLinode is the linode infrastructure provider's name, as
	// clusterctl and --provider know it.
	ProviderLinode = "linode"
	// ProviderGCP is the gcp infrastructure provider's name, as clusterctl and
	// --provider know it.
	ProviderGCP = "gcp"
	// ProviderAzure is the azure infrastructure provider's name, as clusterctl
	// and --provider know it.
	ProviderAzure = "azure"
	// FileName is the configuration file's name inside the config directory.
	FileName = "config.yaml"
	// IdentityFileName is the default age identity file's name inside the
	// config directory.
	IdentityFileName = "identity.txt"
)

// Backend names.
const (
	BackendFile    = "file"
	BackendProxmox = "proxmox"
	BackendS3      = "s3"
)

// Payload host kinds.
const (
	PayloadS3 = "s3"
)

// DefaultPresignTTL is how long an S3 fetch URL stays valid when
// payload.s3.presignTTL is not set.
const DefaultPresignTTL = time.Hour

// Config is the merged configuration.
type Config struct {
	// Backend is "proxmox", "file" or "s3". Empty means the infrastructure
	// provider's native store when it has one and is configured, else file;
	// see EffectiveBackend. "s3" is never inferred and must be set explicitly.
	Backend string `json:"backend,omitempty"`
	// Payload configures the host that serves files too large for a
	// provider's user-data channel. Unset means none.
	Payload Payload `json:"payload,omitempty"`
	// StateDir holds the file backend's records and the credential cache.
	StateDir string `json:"stateDir,omitempty"`
	// S3Backend configures the s3 state backend. It is used only when the
	// effective backend is "s3".
	S3Backend S3Backend `json:"s3,omitempty"`
	// SSHAuthorizedKeys are installed for the bootstrap node's default user.
	SSHAuthorizedKeys []string   `json:"sshAuthorizedKeys,omitempty"`
	Encryption        Encryption `json:"encryption,omitempty"`
	Proxmox           Proxmox    `json:"proxmox,omitempty"`
	AWS               AWS        `json:"aws,omitempty"`
	Linode            Linode     `json:"linode,omitempty"`
	GCP               GCP        `json:"gcp,omitempty"`
	Azure             Azure      `json:"azure,omitempty"`
	Docker            Docker     `json:"docker,omitempty"`
	CAPI              CAPI       `json:"capi,omitempty"`
	// Nodes configures what every node of a generated cluster does at boot.
	Nodes Nodes `json:"nodes,omitempty"`
	// Providers pins infrastructure provider releases by provider name.
	Providers map[string]Provider `json:"providers,omitempty"`
	// Variables are default template variables, below anything the user or
	// the provider sets.
	Variables map[string]string `json:"variables,omitempty"`
	// Profiles are named overrides merged over the top level.
	Profiles map[string]json.RawMessage `json:"profiles,omitempty"`

	// Path is the file the configuration was read from (it may not exist).
	Path string `json:"-"`
	// Profile is the profile that was applied, if any.
	Profile string `json:"-"`
}

// Encryption configures how state records are sealed.
type Encryption struct {
	IdentityFile string   `json:"identityFile,omitempty"`
	Recipients   []string `json:"recipients,omitempty"`
	// Plain stores state unencrypted. Development only: the record then
	// holds the cluster CA key in the clear.
	Plain bool `json:"plain,omitempty"`
}

// AWS holds the AWS connection and placement settings for the aws (CAPA)
// provider. Credentials come from the default chain (env, shared profile,
// instance role); nothing here is a credential.
type AWS struct {
	// Region is the AWS region, e.g. "us-east-1". Required for the aws provider.
	Region string `json:"region,omitempty"`
	// Profile selects a shared-config profile; empty uses the default chain.
	Profile string `json:"profile,omitempty"`
	// VPCID and SubnetID place the bootstrap instance (required; VPC creation
	// is not implemented yet).
	VPCID    string `json:"vpcId,omitempty"`
	SubnetID string `json:"subnetId,omitempty"`
	// SecurityGroupIDs are attached to the bootstrap instance.
	SecurityGroupIDs []string `json:"securityGroupIds,omitempty"`
	// AMIID is the bootstrap image; empty resolves the latest Ubuntu 22.04.
	AMIID string `json:"amiId,omitempty"`
	// InstanceType sizes the bootstrap instance; empty means t3.large.
	InstanceType string `json:"instanceType,omitempty"`
	// SSHKeyName is an existing EC2 key pair for the instance and the template.
	SSHKeyName string `json:"sshKeyName,omitempty"`
	// IAMInstanceProfile is attached to the bootstrap instance.
	IAMInstanceProfile string `json:"iamInstanceProfile,omitempty"`
	// ControlPlaneEndpoint is the API server host (EIP, DNS or LB address).
	// Empty with AllocateElasticIP allocates one.
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// AllocateElasticIP allocates an Elastic IP as the endpoint.
	AllocateElasticIP bool `json:"allocateElasticIp,omitempty"`
	// Port is the API server port; zero means 6443.
	Port int `json:"port,omitempty"`
	// Spot requests Spot instances. Development and test only.
	Spot bool `json:"spot,omitempty"`
	// PublicIP forces a public IP on the bootstrap instance (public subnet,
	// no NAT gateway).
	PublicIP bool `json:"publicIp,omitempty"`
}

// Linode holds the Linode connection and placement settings for the linode
// (CAPL) provider. The API token is not stored here; it comes from the
// environment (LINODE_TOKEN) and is never written back to disk.
type Linode struct {
	// Token is the Linode API token. It comes only from the environment
	// (LINODE_TOKEN), is never accepted from or written to the config file, and
	// is handed to the in-cluster CAPL controller as its credential.
	Token string `json:"-"`
	// Region is the Linode region, e.g. "us-ord". Required for the linode
	// provider. It must support the Metadata service klotho delivers cloud-init
	// through.
	Region string `json:"region,omitempty"`
	// Type sizes the bootstrap instance (and the template's machines); empty
	// means g6-standard-2, the smallest type that clears kubeadm's two-CPU
	// control-plane preflight.
	Type string `json:"type,omitempty"`
	// Image is the bootstrap image; empty means linode/ubuntu22.04 (a plain
	// image, so install-at-boot is needed).
	Image string `json:"image,omitempty"`
	// AuthorizedKeys are SSH public keys installed for the instance's root user,
	// so a failed bootstrap can be diagnosed.
	AuthorizedKeys []string `json:"authorizedKeys,omitempty"`
	// RootPass is the instance root password; empty generates a random one
	// (klotho reaches the node through cloud-init, not the password).
	RootPass string `json:"rootPass,omitempty"`
	// Tags are extra Linode tags added to the bootstrap instance.
	Tags []string `json:"tags,omitempty"`
	// FirewallID attaches an existing Linode Cloud Firewall to the bootstrap
	// instance; zero attaches none.
	FirewallID int `json:"firewallId,omitempty"`
	// ControlPlaneEndpoint is the API server host. Required: klotho mints
	// certificates and node zero's cloud-init before the node boots, and Linode
	// has no free pre-assignable IP. The cheapest choice is a DNS name you
	// control, pointed at node zero after it boots (no NodeBalancer).
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// Port is the API server port; zero means 6443.
	Port int `json:"port,omitempty"`
}

// GCP holds the Google Cloud connection and placement settings for the gcp
// (CAPG) provider. The service-account credential is a file path, not stored
// inline; its contents are handed, base64-encoded, to the in-cluster CAPG
// controller as GCP_B64ENCODED_CREDENTIALS.
type GCP struct {
	// Project is the GCP project ID. Required for the gcp provider.
	Project string `json:"project,omitempty"`
	// Region is the GCP region, e.g. "us-central1". Required.
	Region string `json:"region,omitempty"`
	// Zone is the Compute Engine zone the bootstrap instance is placed in, e.g.
	// "us-central1-a". Required; it must be within Region.
	Zone string `json:"zone,omitempty"`
	// MachineType sizes the bootstrap instance (and the template's machines);
	// empty means e2-medium, the smallest sane type that clears kubeadm's
	// two-CPU control-plane preflight.
	MachineType string `json:"machineType,omitempty"`
	// Image is a fully-qualified image path or a bare image name in ImageProject;
	// empty resolves the newest image in ImageFamily of ImageProject.
	Image string `json:"image,omitempty"`
	// ImageProject and ImageFamily resolve the bootstrap image when Image is
	// empty; empty means ubuntu-os-cloud / ubuntu-2204-lts (a plain image, so
	// install-at-boot is needed).
	ImageProject string `json:"imageProject,omitempty"`
	ImageFamily  string `json:"imageFamily,omitempty"`
	// DiskSizeGB and DiskType size the bootstrap boot disk; zero/empty mean 30 GB
	// pd-balanced.
	DiskSizeGB int64  `json:"diskSizeGB,omitempty"`
	DiskType   string `json:"diskType,omitempty"`
	// Network is the VPC network nodes join; empty means "default". Subnet is the
	// region subnetwork; empty means the network's "default" subnet.
	Network string `json:"network,omitempty"`
	Subnet  string `json:"subnet,omitempty"`
	// ServiceAccount is the instance service-account email; empty uses the
	// project's default compute service account.
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// NetworkTags are GCE network tags applied to the bootstrap instance.
	NetworkTags []string `json:"networkTags,omitempty"`
	// AuthorizedKeys are SSH public keys installed via the instance "ssh-keys"
	// metadata (for LoginUser), so a failed bootstrap can be diagnosed.
	AuthorizedKeys []string `json:"authorizedKeys,omitempty"`
	// LoginUser is the OS user the authorized keys are installed for; empty means
	// "klotho".
	LoginUser string `json:"loginUser,omitempty"`
	// PublicIP gives the bootstrap instance an ephemeral external IP (so it
	// reaches the internet with no Cloud NAT). Unset means true; set it false
	// only with a Cloud NAT and a supplied ControlPlaneEndpoint.
	PublicIP *bool `json:"publicIp,omitempty"`
	// Preemptible requests the bootstrap instance as a Spot VM. Development only;
	// a reclaimed node is terminated, not restarted. The template's CAPG-managed
	// machines are preemptible regardless.
	Preemptible bool `json:"preemptible,omitempty"`
	// ControlPlaneEndpoint is the API server host. Empty means klotho reserves
	// the global static IP CAPG's (unavoidable) load balancer uses, so the
	// endpoint is known before the certificates are minted.
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// Port is the API server port; zero means 6443.
	Port int `json:"port,omitempty"`
	// CredentialsFile is a path to a service-account key JSON. Empty uses
	// Application Default Credentials for klotho's host-side calls (and then the
	// in-cluster controller has no credential unless ADC resolves one it can
	// read). It is read from GOOGLE_APPLICATION_CREDENTIALS when unset.
	CredentialsFile string `json:"credentialsFile,omitempty"`
}

// Azure holds the Azure connection and placement settings for the azure (CAPZ)
// provider. The service principal's client secret is not stored here; it comes
// from the environment (AZURE_CLIENT_SECRET) and is never written back to disk.
type Azure struct {
	// SubscriptionID is the Azure subscription the cluster lives in. Required.
	SubscriptionID string `json:"subscriptionId,omitempty"`
	// TenantID and ClientID identify the service principal klotho authenticates
	// with and hands to the in-cluster CAPZ controller. Required.
	TenantID string `json:"tenantId,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	// ClientSecret is the service principal password. It comes only from the
	// environment (AZURE_CLIENT_SECRET), is never accepted from or written to the
	// config file, and is handed to CAPZ as its credential.
	ClientSecret string `json:"-"`
	// Location is the Azure region, e.g. "eastus". Required.
	Location string `json:"location,omitempty"`
	// ResourceGroup is an existing resource group the bootstrap VM is created in;
	// it is also CAPZ's node resource group. Required (klotho creates no RG).
	ResourceGroup string `json:"resourceGroup,omitempty"`
	// VNet and Subnet are an existing virtual network and subnet in ResourceGroup
	// the bootstrap VM attaches to. Required (klotho creates no network).
	VNet   string `json:"vnet,omitempty"`
	Subnet string `json:"subnet,omitempty"`
	// VMSize sizes the bootstrap VM (and the template's machines); empty means
	// Standard_B2s, the smallest burstable size that clears kubeadm's two-CPU
	// preflight.
	VMSize string `json:"vmSize,omitempty"`
	// ImagePublisher, ImageOffer, ImageSKU and ImageVersion override the
	// bootstrap marketplace image; empty means Ubuntu 22.04 LTS gen2 (a plain
	// image, so install-at-boot is needed).
	ImagePublisher string `json:"imagePublisher,omitempty"`
	ImageOffer     string `json:"imageOffer,omitempty"`
	ImageSKU       string `json:"imageSku,omitempty"`
	ImageVersion   string `json:"imageVersion,omitempty"`
	// AdminUsername is the Linux admin user Azure creates; empty means "klotho".
	AdminUsername string `json:"adminUsername,omitempty"`
	// SSHPublicKey is an OpenSSH public key installed for the admin user, so a
	// failed bootstrap can be diagnosed. Empty sets a random admin password.
	SSHPublicKey string `json:"sshPublicKey,omitempty"`
	// Spot requests Spot instances. Development and test only.
	Spot bool `json:"spot,omitempty"`
	// ControlPlaneEndpoint is the API server host. Empty means node zero's own
	// static public IP (allocated by Prepare) is used — the cheapest endpoint,
	// no public load balancer.
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// Port is the API server port; zero means 6443.
	Port int `json:"port,omitempty"`
}

// Docker holds the Cluster API Docker provider (CAPD) settings for local
// development clusters. CAPD is for testing only, not production.
type Docker struct {
	// Runtime is the container CLI, "docker" (default) or "podman".
	Runtime string `json:"runtime,omitempty"`
	// NodeImage overrides the kindest/node image for the bootstrap container
	// (for example "kindest/node:v1.33.0"). Empty derives it from the
	// cluster's KUBERNETES_VERSION variable.
	NodeImage string `json:"nodeImage,omitempty"`
	// Network is the Docker network nodes join. Empty means "kind".
	Network string `json:"network,omitempty"`
	// LBImage overrides the haproxy load-balancer image. Empty means the CAPD
	// default.
	LBImage string `json:"lbImage,omitempty"`
	// Socket is the host container-runtime socket mounted into every node so
	// the in-cluster CAPD manager can manage sibling containers. For podman,
	// "/run/user/<uid>/podman/podman.sock"; for docker, "/var/run/docker.sock".
	// It is also mounted into the deletion cleanup container.
	Socket string `json:"socket,omitempty"`
	// CleanupImage overrides the image the deletion cleanup container runs. It
	// must carry a Docker-API-compatible CLI and a POSIX shell. Empty means the
	// default (the official docker CLI image).
	CleanupImage string `json:"cleanupImage,omitempty"`
}

// Proxmox holds the Proxmox VE connection and placement settings.
type Proxmox struct {
	URL     string `json:"url,omitempty"`
	TokenID string `json:"tokenId,omitempty"`
	Secret  string `json:"secret,omitempty"`
	// AdditionalURLs are further API URLs of the same Proxmox cluster. The
	// Cluster API Proxmox provider in the cluster uses them when the node
	// behind URL is down or cannot reach the node a request is about.
	AdditionalURLs []string `json:"additionalUrls,omitempty"`
	// EndpointDiscovery makes the provider add every online node of the
	// Proxmox cluster as such an endpoint, read from the cluster status
	// through URL (the token needs Sys.Audit on /).
	EndpointDiscovery bool   `json:"endpointDiscovery,omitempty"`
	Insecure          bool   `json:"insecure,omitempty"`
	CACertFile        string `json:"caCertFile,omitempty"`
	Node              string `json:"node,omitempty"`
	TemplateID        int    `json:"templateId,omitempty"`
	TemplateTag       string `json:"templateTag,omitempty"`
	// Template selects the VM template by tags instead of by VMID, for the
	// bootstrap node and for every machine template of a generated bundle.
	Template   ProxmoxTemplate `json:"template,omitempty"`
	ISOStorage string          `json:"isoStorage,omitempty"`
	Bridge     string          `json:"bridge,omitempty"`
	Cores      int             `json:"cores,omitempty"`
	Sockets    int             `json:"sockets,omitempty"`
	MemoryMiB  int             `json:"memoryMiB,omitempty"`
	VMIDRange  []int           `json:"vmidRange,omitempty"`
	Pool       string          `json:"pool,omitempty"`
	// Sidecar tunes the token-only Alpine LXC that a default (fire-and-forget)
	// delete launches to finish a cluster's teardown after klotho exits. Every
	// field is optional; the zero value is fully defaulted. Use `klotho delete
	// --watch` to sweep in-process instead of launching a sidecar.
	Sidecar ProxmoxSidecar `json:"sidecar,omitempty"`
}

// ProxmoxSidecar tunes the deletion sidecar. See proxmox.SidecarConfig.
type ProxmoxSidecar struct {
	// TemplateStorage receives the one-shot Alpine template and the sidecar's
	// rootfs. Empty auto-selects an active vztmpl- and rootdir-capable storage.
	TemplateStorage string `json:"templateStorage,omitempty"`
	// RootfsStorage overrides where the sidecar's rootfs is allocated. Empty
	// means TemplateStorage.
	RootfsStorage string `json:"rootfsStorage,omitempty"`
	// Bridge is the network bridge the sidecar attaches to. Empty means the
	// provider's bridge, then vmbr0.
	Bridge string `json:"bridge,omitempty"`
	// IP, Gateway and DNS statically address the sidecar when this environment
	// has no DHCP. IP is an address with an optional prefix
	// ("192.168.1.221/20" or "192.168.1.221", prefix then taken from
	// IP_PREFIX). Empty IP derives a free address from the cluster's
	// NODE_IP_RANGES/GATEWAY/IP_PREFIX/DNS_SERVERS, and falls back to DHCP when
	// none is available. Gateway and DNS default to GATEWAY and the first
	// DNS_SERVERS entry.
	IP      string `json:"ip,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	DNS     string `json:"dns,omitempty"`
	// CurlURL/CurlSHA256 and JqURL/JqSHA256 override the static curl and jq
	// binaries klotho bakes into the sidecar so the sweep needs no package
	// install and no internet. Empty means pinned defaults; set a URL with its
	// SHA-256.
	CurlURL    string `json:"curlUrl,omitempty"`
	CurlSHA256 string `json:"curlSha256,omitempty"`
	JqURL      string `json:"jqUrl,omitempty"`
	JqSHA256   string `json:"jqSha256,omitempty"`
	// AlpineRootfsURL and AlpineRootfsSHA256 override the Alpine minirootfs
	// klotho fetches and bakes the sweep into. Set both together.
	AlpineRootfsURL    string `json:"alpineRootfsUrl,omitempty"`
	AlpineRootfsSHA256 string `json:"alpineRootfsSha256,omitempty"`
	// TokenTTL bounds the scoped token minted for the sidecar, as a Go
	// duration (for example "30m"). Empty means a built-in default.
	TokenTTL string `json:"tokenTtl,omitempty"`
	// MemoryMiB sizes the sidecar container. Zero means a small default.
	MemoryMiB int `json:"memoryMiB,omitempty"`
}

// ProxmoxTemplate selects VM templates by tag. With Tags set, klotho clones
// the bootstrap node from the template on its node that matches, and a
// generated bundle's machine templates get a templateSelector with the same
// tags in place of a fixed templateID and sourceNode, so the Cluster API
// Proxmox provider looks the template up on whichever node it schedules each
// machine to. A template carrying the tags must therefore exist on every
// allowed node. A fixed templateId may be set alongside, as long as it is the
// template the tags select on the bootstrap node.
type ProxmoxTemplate struct {
	// Tags are the Proxmox tags a template must carry.
	Tags []string `json:"tags,omitempty"`
	// MatchPolicy is exact (the template's tags equal Tags; the default),
	// uniqueSubset (the only template carrying all of Tags, extras allowed)
	// or bestSubset (of those carrying all of Tags, the one with the fewest
	// extras).
	MatchPolicy string `json:"matchPolicy,omitempty"`
}

// Selects reports whether templates are selected by tag.
func (t ProxmoxTemplate) Selects() bool { return len(t.Tags) > 0 }

// Nodes configures the boot-time setup of a generated cluster's nodes. None
// of it touches a user-supplied manifest.
type Nodes struct {
	// InstallKubernetes adds a script to the generated bundle that installs
	// containerd and the KubeadmControlPlane's Kubernetes version on every
	// node before kubeadm runs, so plain distribution images work as
	// templates. A node that already has that kubeadm skips it. The
	// --install-kubernetes flag overrides this.
	InstallKubernetes bool `json:"installKubernetes,omitempty"`
	// Cache points the install script at local caches. It has no effect
	// without InstallKubernetes.
	Cache Cache `json:"cache,omitempty"`
}

// Cache names local package and image caches the install script uses.
type Cache struct {
	// AptProxy is the URL of an apt proxy such as apt-cacher-ng,
	// http://host:3142.
	AptProxy string `json:"aptProxy,omitempty"`
	// RegistryMirrors maps a registry host (registry.k8s.io, docker.io) to
	// the mirror URLs containerd pulls from first.
	RegistryMirrors map[string][]string `json:"registryMirrors,omitempty"`
}

// InstallOptions returns the cache settings in the form the bundle patch
// takes.
func (n Nodes) InstallOptions() generate.InstallKubernetesOptions {
	return generate.InstallKubernetesOptions{AptProxy: n.Cache.AptProxy, RegistryMirrors: n.Cache.RegistryMirrors}
}

// Payload selects and configures a payload host.
type Payload struct {
	// Kind is "s3", or empty for no payload host.
	Kind string `json:"kind,omitempty"`
	S3   S3     `json:"s3,omitempty"`
}

// S3 configures the S3 payload host: AWS S3 or anything speaking its API,
// such as MinIO, Ceph RGW or Cloudflare R2.
type S3 struct {
	// Endpoint is the store's URL; empty means AWS S3.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the signing region; empty means us-east-1, which most
	// non-AWS stores accept.
	Region string `json:"region,omitempty"`
	// Bucket must exist; klotho does not create it.
	Bucket string `json:"bucket,omitempty"`
	// Prefix is a key prefix below which everything is published.
	Prefix string `json:"prefix,omitempty"`
	// UsePathStyle puts the bucket in the URL path rather than the host
	// name; MinIO needs it.
	UsePathStyle bool `json:"usePathStyle,omitempty"`
	// PresignTTL is how long a fetch URL stays valid, as a duration such as
	// 1h or 30m; see DefaultPresignTTL.
	PresignTTL string `json:"presignTTL,omitempty"`
	// AccessKeyID and SecretAccessKey come only from the environment
	// (EnvS3AccessKeyID, EnvS3SecretKey). Unset, the standard AWS
	// credential chain applies.
	AccessKeyID     string `json:"-"`
	SecretAccessKey string `json:"-"`
}

// TTL returns the presign lifetime, or DefaultPresignTTL when unset. Load
// has already validated the value.
func (s S3) TTL() time.Duration {
	if s.PresignTTL == "" {
		return DefaultPresignTTL
	}
	d, err := time.ParseDuration(s.PresignTTL)
	if err != nil || d <= 0 {
		return DefaultPresignTTL
	}
	return d
}

// S3Backend configures the s3 state backend: AWS S3 or anything speaking its
// API, such as MinIO, Ceph RGW or Cloudflare R2. The backend stores each
// cluster's already-sealed state record as one object; it never holds a
// plaintext state or a usable credential.
type S3Backend struct {
	// Endpoint is the store's URL; empty means AWS S3.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the signing region; empty means us-east-1, which most
	// non-AWS stores accept.
	Region string `json:"region,omitempty"`
	// Bucket must exist; klotho does not create it. Required when backend is s3.
	Bucket string `json:"bucket,omitempty"`
	// Prefix is a key prefix below which every record is stored.
	Prefix string `json:"prefix,omitempty"`
	// UsePathStyle puts the bucket in the URL path rather than the host name;
	// MinIO needs it.
	UsePathStyle bool `json:"usePathStyle,omitempty"`
	// AccessKeyID and SecretAccessKey come only from the environment
	// (EnvS3AccessKeyID, EnvS3SecretKey). Unset, the standard AWS credential
	// chain (environment, shared config, instance role) applies.
	AccessKeyID     string `json:"-"`
	SecretAccessKey string `json:"-"`
}

// problems lists what is wrong with an S3 backend section.
func (s S3Backend) problems() []string {
	var problems []string
	if s.Bucket == "" {
		problems = append(problems, "s3.bucket is required when backend is s3")
	}
	if s.Endpoint != "" {
		u, err := url.Parse(s.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			problems = append(problems, fmt.Sprintf("s3.endpoint %q must be an http(s) URL", s.Endpoint))
		}
	}
	if (s.AccessKeyID == "") != (s.SecretAccessKey == "") {
		problems = append(problems, fmt.Sprintf("%s and %s must be set together", EnvS3AccessKeyID, EnvS3SecretKey))
	}
	return problems
}

// CAPI configures the Cluster API core, bootstrap and control plane
// providers the operator installs, and the bootstrap node's add-ons.
type CAPI struct {
	Version string `json:"version,omitempty"`
	// FetchURL overrides the release directory the core, bootstrap and
	// control plane components are fetched from, e.g.
	// https://github.com/<owner>/cluster-api/releases/<version>/.
	FetchURL       string          `json:"fetchURL,omitempty"`
	FeatureGates   map[string]bool `json:"featureGates,omitempty"`
	OperatorURL    string          `json:"operatorURL,omitempty"`
	CertManagerURL string          `json:"certManagerURL,omitempty"`
	CNIManifestURL string          `json:"cniManifestURL,omitempty"`
}

// Provider pins an infrastructure provider release.
type Provider struct {
	Version string `json:"version,omitempty"`
	// FetchURL overrides the provider's infrastructure-components.yaml URL.
	// klotho reads the cluster template from the same release directory, so
	// this is also where a local flat cluster-template.yaml is picked up; a
	// file:// URL (a local clusterctl repository) works here but is not
	// reachable by the in-cluster operator - see ComponentsURL.
	FetchURL string `json:"fetchURL,omitempty"`
	// ComponentsURL overrides only the infrastructure-components.yaml URL the
	// in-cluster Cluster API operator fetches, leaving FetchURL for the
	// host-side template read. It exists because the two have different
	// reachability requirements: the template read happens on the host (so
	// FetchURL may be a local file:// repository carrying a flat template the
	// released provider does not ship), while the operator runs inside the new
	// cluster and needs an http(s)/oci URL it can reach, such as the provider's
	// real release. Empty means the operator uses FetchURL.
	ComponentsURL string `json:"componentsURL,omitempty"`
	// Image replaces the provider manager's container image. Needed with a
	// FetchURL that points at another repository's release: the operator
	// keeps the image repository it knows for the provider and only follows
	// the version tag.
	Image string `json:"image,omitempty"`
}

// Dir returns klotho's configuration directory: $XDG_CONFIG_HOME/klotho, or
// ~/.config/klotho.
func Dir() string {
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "klotho")
}

// DefaultPath returns the default configuration file path.
func DefaultPath() string { return filepath.Join(Dir(), FileName) }

// DefaultStateDir returns $XDG_STATE_HOME/klotho, or ~/.local/state/klotho.
func DefaultStateDir() string {
	return filepath.Join(xdg("XDG_STATE_HOME", filepath.Join(".local", "state")), "klotho")
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return filepath.Join(home(), fallback)
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

// Expand replaces a leading "~" with the home directory and expands
// environment variables. $XDG_CONFIG_HOME and $XDG_STATE_HOME fall back to
// their defaults when unset.
func Expand(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" {
		p = home()
	} else if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = filepath.Join(home(), rest)
	} else if rest, ok := strings.CutPrefix(p, `~\`); ok {
		p = filepath.Join(home(), rest)
	}
	p = os.Expand(p, func(name string) string {
		switch name {
		case "XDG_CONFIG_HOME":
			return xdg(name, ".config")
		case "XDG_STATE_HOME":
			return xdg(name, filepath.Join(".local", "state"))
		case "HOME":
			if v := os.Getenv("HOME"); v != "" {
				return v
			}
			return home()
		}
		return os.Getenv(name)
	})
	return filepath.Clean(p)
}

// Load reads the configuration at path (DefaultPath when empty), merges the
// named profile over it, applies environment overrides and defaults, and
// validates the result. A missing file is not an error; an unknown profile
// is.
func Load(path, profile string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	path = Expand(path)
	raw := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("config: %w", err)
	default:
		if err := yaml.Unmarshal(b, &raw); err != nil {
			return nil, fmt.Errorf("config: %s: %w", path, err)
		}
		if raw == nil {
			raw = map[string]any{}
		}
	}

	if profile != "" {
		profiles, _ := raw["profiles"].(map[string]any)
		p, ok := profiles[profile]
		if !ok {
			return nil, fmt.Errorf("config: %s: profile %q is not defined%s", path, profile, available(profiles))
		}
		pm, ok := p.(map[string]any)
		if !ok && p != nil {
			return nil, fmt.Errorf("config: %s: profile %q is not a mapping", path, profile)
		}
		if _, nested := pm["profiles"]; nested {
			return nil, fmt.Errorf("config: %s: profile %q may not define profiles", path, profile)
		}
		merge(raw, pm)
	}

	js, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	c := &Config{}
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	c.Path = path
	c.Profile = profile
	if err := c.applyEnv(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func available(profiles map[string]any) string {
	if len(profiles) == 0 {
		return " (no profiles are defined)"
	}
	names := make([]string, 0, len(profiles))
	for n := range profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return " (defined: " + strings.Join(names, ", ") + ")"
}

// merge deep-merges src into dst. Maps merge; everything else, lists
// included, replaces.
func merge(dst, src map[string]any) {
	for k, v := range src {
		sm, sok := v.(map[string]any)
		dm, dok := dst[k].(map[string]any)
		if sok && dok {
			merge(dm, sm)
			continue
		}
		dst[k] = v
	}
}

func (c *Config) applyEnv() error {
	if v, ok := os.LookupEnv(EnvBackend); ok && v != "" {
		c.Backend = v
	}
	if v, ok := os.LookupEnv(EnvStateDir); ok && v != "" {
		c.StateDir = v
	}
	if v, ok := os.LookupEnv(EnvIdentityFile); ok && v != "" {
		c.Encryption.IdentityFile = v
	}
	if v, ok := os.LookupEnv(EnvRecipients); ok && v != "" {
		var rs []string
		for _, r := range strings.Split(v, ",") {
			if r = strings.TrimSpace(r); r != "" {
				rs = append(rs, r)
			}
		}
		c.Encryption.Recipients = rs
	}
	if v, ok := os.LookupEnv(EnvProxmoxURL); ok && v != "" {
		c.Proxmox.URL = v
	}
	if v, ok := os.LookupEnv(EnvProxmoxTokenID); ok && v != "" {
		c.Proxmox.TokenID = v
	}
	if v, ok := os.LookupEnv(EnvProxmoxSecret); ok && v != "" {
		c.Proxmox.Secret = v
	}
	if v, ok := os.LookupEnv(EnvProxmoxInsecure); ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: %s=%q is not a boolean", EnvProxmoxInsecure, v)
		}
		c.Proxmox.Insecure = b
	}
	if v, ok := os.LookupEnv(EnvS3AccessKeyID); ok && v != "" {
		c.Payload.S3.AccessKeyID = v
		c.S3Backend.AccessKeyID = v
	}
	if v, ok := os.LookupEnv(EnvS3SecretKey); ok && v != "" {
		c.Payload.S3.SecretAccessKey = v
		c.S3Backend.SecretAccessKey = v
	}
	if v, ok := os.LookupEnv(EnvLinodeToken); ok && v != "" {
		c.Linode.Token = v
	}
	if c.GCP.CredentialsFile == "" {
		if v, ok := os.LookupEnv(EnvGCPCredentials); ok && v != "" {
			c.GCP.CredentialsFile = v
		}
	}
	if v, ok := os.LookupEnv(EnvAzureClientSecret); ok && v != "" {
		c.Azure.ClientSecret = v
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir()
	}
	c.StateDir = Expand(c.StateDir)
	if c.Encryption.IdentityFile == "" {
		c.Encryption.IdentityFile = filepath.Join(Dir(), IdentityFileName)
	}
	c.Encryption.IdentityFile = Expand(c.Encryption.IdentityFile)
	c.Proxmox.CACertFile = Expand(c.Proxmox.CACertFile)
	if c.Proxmox.Template.Selects() && c.Proxmox.Template.MatchPolicy == "" {
		c.Proxmox.Template.MatchPolicy = generate.TemplateMatchExact
	}
	if c.CAPI.Version == "" {
		c.CAPI.Version = DefaultCAPIVersion
	}
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	p := c.Providers[BackendProxmox]
	if p.Version == "" {
		p.Version = DefaultProxmoxVersion
	}
	c.Providers[BackendProxmox] = p
	aws := c.Providers[ProviderAWS]
	if aws.Version == "" {
		aws.Version = DefaultAWSVersion
	}
	c.Providers[ProviderAWS] = aws
	linode := c.Providers[ProviderLinode]
	if linode.Version == "" {
		linode.Version = DefaultLinodeVersion
	}
	c.Providers[ProviderLinode] = linode
	gcp := c.Providers[ProviderGCP]
	if gcp.Version == "" {
		gcp.Version = DefaultGCPVersion
	}
	c.Providers[ProviderGCP] = gcp
	c.GCP.CredentialsFile = Expand(c.GCP.CredentialsFile)
	azure := c.Providers[ProviderAzure]
	if azure.Version == "" {
		azure.Version = DefaultAzureVersion
	}
	c.Providers[ProviderAzure] = azure
	if c.Variables == nil {
		c.Variables = map[string]string{}
	}
}

// Validate checks values that can be checked without touching the network.
func (c *Config) Validate() error {
	var problems []string
	switch c.Backend {
	case "", BackendFile, BackendProxmox:
	case BackendS3:
		problems = append(problems, c.S3Backend.problems()...)
	default:
		problems = append(problems, fmt.Sprintf("backend %q is not one of %s, %s, %s", c.Backend, BackendProxmox, BackendFile, BackendS3))
	}
	switch c.Payload.Kind {
	case "":
	case PayloadS3:
		problems = append(problems, c.Payload.S3.problems()...)
	default:
		problems = append(problems, fmt.Sprintf("payload.kind %q is not one of %s", c.Payload.Kind, PayloadS3))
	}
	if len(c.Encryption.Recipients) > 0 {
		if _, err := state.ParseRecipients(c.Encryption.Recipients); err != nil {
			problems = append(problems, fmt.Sprintf("encryption.recipients (or %s): %v", EnvRecipients, err))
		}
	}
	if r := c.Proxmox.VMIDRange; len(r) != 0 && (len(r) != 2 || r[0] <= 0 || r[1] < r[0]) {
		problems = append(problems, fmt.Sprintf("proxmox.vmidRange %v must be [start, end] with 0 < start <= end", r))
	}
	problems = append(problems, c.Proxmox.Template.problems()...)
	if err := c.Nodes.InstallOptions().Validate(); err != nil {
		problems = append(problems, "nodes.cache: "+err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: %s: %s", c.Path, strings.Join(problems, "; "))
	}
	return nil
}

// problems lists what is wrong with a template selection.
func (t ProxmoxTemplate) problems() []string {
	var problems []string
	if t.Selects() {
		for _, tag := range t.Tags {
			if !generate.TemplateTagPattern.MatchString(tag) {
				problems = append(problems, fmt.Sprintf("proxmox.template.tags: %q is not a Proxmox tag (letters, digits, _ . + -)", tag))
			}
		}
	} else if t.MatchPolicy != "" {
		problems = append(problems, "proxmox.template.matchPolicy needs proxmox.template.tags")
	}
	if _, err := generate.NormalizeTemplateMatchPolicy(t.MatchPolicy); err != nil {
		problems = append(problems, "proxmox.template.matchPolicy: "+err.Error())
	}
	return problems
}

// problems lists what is wrong with an S3 payload section.
func (s S3) problems() []string {
	var problems []string
	if s.Bucket == "" {
		problems = append(problems, "payload.s3.bucket is required")
	}
	if s.Endpoint != "" {
		u, err := url.Parse(s.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			problems = append(problems, fmt.Sprintf("payload.s3.endpoint %q must be an http(s) URL", s.Endpoint))
		}
	}
	if s.PresignTTL != "" {
		d, err := time.ParseDuration(s.PresignTTL)
		if err != nil || d <= 0 {
			problems = append(problems, fmt.Sprintf("payload.s3.presignTTL %q must be a positive duration such as 1h", s.PresignTTL))
		}
	}
	if (s.AccessKeyID == "") != (s.SecretAccessKey == "") {
		problems = append(problems, fmt.Sprintf("%s and %s must be set together", EnvS3AccessKeyID, EnvS3SecretKey))
	}
	return problems
}

// PayloadConfigured reports whether a payload host is configured.
func (c *Config) PayloadConfigured() bool { return c.Payload.Kind != "" }

// ProxmoxConfigured reports whether a Proxmox URL is set.
func (c *Config) ProxmoxConfigured() bool { return c.Proxmox.URL != "" }

// CheckProxmox reports which Proxmox connection settings are missing, naming
// the environment variable that supplies each.
func (c *Config) CheckProxmox() error {
	var missing []string
	if c.Proxmox.URL == "" {
		missing = append(missing, "proxmox.url ("+EnvProxmoxURL+")")
	}
	if c.Proxmox.TokenID == "" {
		missing = append(missing, "proxmox.tokenId ("+EnvProxmoxTokenID+")")
	}
	if c.Proxmox.Secret == "" {
		missing = append(missing, "proxmox.secret ("+EnvProxmoxSecret+")")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: Proxmox connection is incomplete: set %s", strings.Join(missing, ", "))
	}
	return nil
}

// EffectiveBackend returns the backend to use: the configured one, else
// proxmox when a Proxmox URL is configured (the Proxmox provider's native
// store), else file.
func (c *Config) EffectiveBackend() string {
	if c.Backend != "" {
		return c.Backend
	}
	if c.ProxmoxConfigured() {
		return BackendProxmox
	}
	return BackendFile
}

// RecordsDir is where the file backend keeps records.
func (c *Config) RecordsDir() string { return filepath.Join(c.StateDir, "clusters") }

// CredentialsDir is where minted client credentials are cached.
func (c *Config) CredentialsDir() string { return filepath.Join(c.StateDir, "credentials") }

// ShellDir is where the shell command writes its temporary kubeconfigs.
func (c *Config) ShellDir() string { return filepath.Join(c.StateDir, "shell") }

// Codec builds the record codec: PlainCodec when encryption.plain is set,
// otherwise an age codec that encrypts to the configured recipients (or to
// the identities' own recipients when none are configured) and decrypts with
// the identities in the identity file.
func (c *Config) Codec() (state.RecordCodec, error) {
	if c.Encryption.Plain {
		return state.PlainCodec{}, nil
	}
	var ids []age.Identity
	idPath := c.Encryption.IdentityFile
	if _, err := os.Stat(idPath); err == nil {
		if ids, err = state.LoadIdentitiesFromFile(idPath); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config: identity file: %w", err)
	}
	var recipients []age.Recipient
	if len(c.Encryption.Recipients) > 0 {
		rs, err := state.ParseRecipients(c.Encryption.Recipients)
		if err != nil {
			return nil, fmt.Errorf("config: encryption.recipients (or %s): %w", EnvRecipients, err)
		}
		recipients = rs
	} else {
		for _, id := range ids {
			x, ok := id.(*age.X25519Identity)
			if !ok {
				return nil, fmt.Errorf("config: identity file %s holds a non-X25519 identity; set encryption.recipients (%s) explicitly", idPath, EnvRecipients)
			}
			recipients = append(recipients, x.Recipient())
		}
	}
	if len(recipients) == 0 && len(ids) == 0 {
		return nil, fmt.Errorf("config: no age identity at %s and no recipients configured; run \"klotho init\" or set %s", idPath, EnvIdentityFile)
	}
	codec, err := state.NewCodec(recipients, ids)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return codec, nil
}

// Init creates the age identity file at identityFile (mode 0600, parent
// directories 0700) if it does not exist, and returns the recipient (public
// key) of its first X25519 identity. It is idempotent.
func Init(identityFile string) (publicKey string, err error) {
	if identityFile == "" {
		identityFile = filepath.Join(Dir(), IdentityFileName)
	}
	identityFile = Expand(identityFile)
	if _, err := os.Stat(identityFile); err == nil {
		ids, err := state.LoadIdentitiesFromFile(identityFile)
		if err != nil {
			return "", fmt.Errorf("config: %w", err)
		}
		for _, id := range ids {
			if x, ok := id.(*age.X25519Identity); ok {
				return x.Recipient().String(), nil
			}
		}
		return "", fmt.Errorf("config: identity file %s has no X25519 identity", identityFile)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(identityFile), 0o700); err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	id, err := state.GenerateIdentity()
	if err != nil {
		return "", err
	}
	body := "# klotho state identity, created by \"klotho init\"\n" +
		"# public key: " + id.Recipient().String() + "\n" +
		id.String() + "\n"
	f, err := os.OpenFile(identityFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(identityFile)
		return "", fmt.Errorf("config: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	return id.Recipient().String(), nil
}

// Skeleton is the commented configuration file "klotho init" writes when
// none exists.
const Skeleton = `# klotho configuration. Every field is optional; see "klotho --help".
# backend: proxmox            # or file, s3; default: proxmox when proxmox.url is set
# stateDir: ~/.local/state/klotho
# s3:                        # state backend; used only when backend: s3
#   endpoint: http://minio.example:9000   # omit for AWS S3
#   region: us-east-1
#   bucket: klotho-state
#   prefix: clusters
#   usePathStyle: true       # needed by MinIO
#   # keys: set KLOTHO_S3_ACCESS_KEY_ID and KLOTHO_S3_SECRET_ACCESS_KEY,
#   # or leave both unset to use the AWS credential chain
# encryption:
#   identityFile: ~/.config/klotho/identity.txt
#   recipients: []           # default: the identity file's public keys
# proxmox:
#   url: https://pve.example:8006
#   tokenId: user@pam!klotho
#   # secret: set KLOTHO_PROXMOX_SECRET instead of storing it here
#   additionalUrls: []       # other nodes' API URLs for the in-cluster provider to fall back to
#   endpointDiscovery: false # true: the provider adds every online node itself (needs Sys.Audit on /)
#   templateId: 9000
#   template:                # or select templates by tag, on every allowed node
#     tags: [k8s, ubuntu]
#     matchPolicy: exact     # exact | uniqueSubset | bestSubset
#   vmidRange: [9100, 9199]
# nodes:
#   installKubernetes: false # true: install containerd and Kubernetes at boot (plain images)
#   cache:                   # used by the install script only
#     aptProxy: http://cache.example:3142
#     registryMirrors:
#       registry.k8s.io: [http://cache.example:5000]
# payload:                   # where files too large for user-data are served from
#   kind: s3
#   s3:
#     endpoint: http://minio.example:9000   # omit for AWS S3
#     region: us-east-1
#     bucket: klotho
#     prefix: payload
#     usePathStyle: true     # needed by MinIO
#     presignTTL: 1h
#     # keys: set KLOTHO_S3_ACCESS_KEY_ID and KLOTHO_S3_SECRET_ACCESS_KEY,
#     # or leave both unset to use the AWS credential chain
# capi:
#   version: v1.12.9
# providers:
#   proxmox:
#     version: v0.9.2
#     # a release of another repository (a fork): its components URL and, because the
#     # operator keeps the image repository it knows, the manager image to run
#     # fetchURL: https://github.com/<owner>/cluster-api-provider-proxmox/releases/<version>/infrastructure-components.yaml
#     # image: ghcr.io/<owner>/cluster-api-provider-proxmox:<version>
# variables:
#   CONTROL_PLANE_ENDPOINT_IP: 10.0.0.10
# profiles:
#   lab:
#     proxmox:
#       url: https://lab:8006
`

// WriteSkeleton writes Skeleton to path unless a file already exists there.
// It reports whether it wrote one.
func WriteSkeleton(path string) (bool, error) {
	if path == "" {
		path = DefaultPath()
	}
	path = Expand(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("config: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("config: %w", err)
	}
	if _, err := f.WriteString(Skeleton); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("config: %w", err)
	}
	return true, f.Close()
}
