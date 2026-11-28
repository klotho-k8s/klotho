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
//	KLOTHO_LIBVIRT_URI          libvirt.uri
//	KLOTHO_VZ_TOKEN             vz.token
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
	"github.com/klotho-k8s/klotho/pkg/hooks"
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
	// EnvVaultAddr and EnvVaultToken are the standard Vault environment
	// variables. They supply the vault state backend's address and token; the
	// token is never accepted from or written to the config file.
	EnvVaultAddr      = "VAULT_ADDR"
	EnvVaultToken     = "VAULT_TOKEN"
	EnvVaultNamespace = "VAULT_NAMESPACE"
	EnvLinodeToken     = "LINODE_TOKEN"
	// EnvDOToken is the DigitalOcean API token; it sets digitalocean.token when
	// that is unset. klotho never writes it back to disk.
	EnvDOToken = "DIGITALOCEAN_TOKEN"
	// EnvGCPCredentials is the standard GCP service-account key path; klotho
	// reads it for gcp.credentialsFile when that is unset.
	EnvGCPCredentials = "GOOGLE_APPLICATION_CREDENTIALS"
	// EnvAzureClientSecret is the Azure service-principal client secret.
	EnvAzureClientSecret = "AZURE_CLIENT_SECRET"
	// EnvLibvirtURI is the libvirt connection URI; it sets libvirt.uri when that
	// is unset.
	EnvLibvirtURI = "KLOTHO_LIBVIRT_URI"
	// EnvVZToken is the capvz-daemon bearer token; it sets vz.token when that is
	// unset. klotho never writes it back to disk.
	EnvVZToken = "KLOTHO_VZ_TOKEN"
	// EnvHetznerToken is the Hetzner Cloud API token; it sets hetzner.token when
	// that is unset. klotho never writes it back to disk.
	EnvHetznerToken = "HCLOUD_TOKEN"
	// EnvVSphereUsername and EnvVSpherePassword are the vCenter credentials; they
	// set vsphere.username / vsphere.password when those are unset. klotho never
	// writes the password back to disk.
	EnvVSphereUsername = "VSPHERE_USERNAME"
	EnvVSpherePassword = "VSPHERE_PASSWORD"
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
	// DefaultDOVersion is the Cluster API Provider DigitalOcean (CAPDO) release
	// installed for the digitalocean provider. CAPDO's infrastructure objects are
	// infrastructure.cluster.x-k8s.io/v1beta1; the flat cluster template authors
	// the CAPI-core objects in the v1beta2 contract klotho requires. VERIFY the
	// release and the contract compatibility against a live run.
	DefaultDOVersion = "v1.7.0"
	// DefaultAzureVersion is the Cluster API Provider Azure (CAPZ) release
	// installed for the azure provider. CAPZ v1.23.x is built against Cluster
	// API v1.12.x (DefaultCAPIVersion), and its infrastructure CRDs are
	// infrastructure.cluster.x-k8s.io/v1beta1. VERIFY against a live run.
	DefaultAzureVersion = "v1.23.3"
	// DefaultOCIVersion is the Cluster API Provider for OCI (CAPOCI) release
	// installed for the oci provider. v0.25.0 is the current release; its
	// infrastructure CRDs are infrastructure.cluster.x-k8s.io/v1beta2 (the
	// storage version) and it is built against Cluster API v1.13.x, so VERIFY the
	// core v1beta2 contract compatibility against a live run.
	DefaultOCIVersion = "v0.25.0"
	// ProviderAWS is the aws infrastructure provider's name, as clusterctl
	// and --provider know it.
	ProviderAWS = "aws"
	// ProviderLinode is the linode infrastructure provider's name, as
	// clusterctl and --provider know it.
	ProviderLinode = "linode"
	// ProviderGCP is the gcp infrastructure provider's name, as clusterctl and
	// --provider know it.
	ProviderGCP = "gcp"
	// ProviderDO is the digitalocean infrastructure provider's name, as
	// clusterctl and --provider know it.
	ProviderDO = "digitalocean"
	// DefaultOpenStackVersion is the Cluster API Provider OpenStack (CAPO) release
	// installed for the openstack provider. v0.15.x is the first line serving the
	// infrastructure.cluster.x-k8s.io/v1beta2 API the flat template is authored in
	// (storage version), with v1beta1 still served via conversion webhooks. VERIFY
	// against a live run.
	DefaultOpenStackVersion = "v0.15.2"
	// ProviderOpenStack is the openstack infrastructure provider's name, as
	// clusterctl and --provider know it.
	ProviderOpenStack = "openstack"
	// ProviderAzure is the azure infrastructure provider's name, as clusterctl
	// and --provider know it.
	ProviderAzure = "azure"
	// ProviderOCI is the oci infrastructure provider's name, as clusterctl and
	// --provider know it.
	ProviderOCI = "oci"
	// DefaultHyperVVersion is the Cluster API Hyper-V provider release klotho
	// installs for the hyperv provider. It is a pre-release; the controller
	// image and release manifests are produced from
	// klotho-k8s/cluster-api-provider-hyperv. v0.1.1 carries the CRD contract
	// labels the operator needs to adopt the provider's objects; v0.1.2 adds the
	// atomic, disconnect-tolerant DeleteVM so a graceful delete leaves no orphan;
	// v0.1.3 wires the HYPERV_AGENT_* env into the controller and reads the agent
	// CA inline, so the manager runs as a Linux pod driving the host agent.
	DefaultHyperVVersion = "v0.1.3"
	// ProviderHyperV is the hyperv infrastructure provider's name, as
	// clusterctl and --provider know it.
	ProviderHyperV = "hyperv"
	// DefaultLibvirtVersion is the Cluster API libvirt provider release klotho
	// installs for the libvirt provider. It is produced from
	// klotho-k8s/cluster-api-provider-libvirt; libvirt is not in clusterctl's
	// built-in provider list, so providers.libvirt.fetchURL (and componentsURL)
	// must point at its release. VERIFY against a live run.
	DefaultLibvirtVersion = "v0.1.0"
	// ProviderLibvirt is the libvirt infrastructure provider's name, as
	// clusterctl and --provider know it.
	ProviderLibvirt = "libvirt"
	// DefaultVZVersion is the Cluster API vz provider release klotho installs for
	// the vz provider. It is produced from klotho-k8s/cluster-api-provider-vz; vz
	// is not in clusterctl's built-in provider list, so providers.vz.fetchURL
	// (and componentsURL) must point at its release. VERIFY against a live run.
	DefaultVZVersion = "v0.1.0"
	// ProviderVZ is the vz (Apple Virtualization.framework) infrastructure
	// provider's name, as clusterctl and --provider know it.
	ProviderVZ = "vz"
	// DefaultHetznerVersion is the Cluster API Provider Hetzner (CAPH) release
	// installed for the hetzner provider. CAPH's infrastructure CRDs are
	// infrastructure.cluster.x-k8s.io/v1beta1; the flat cluster template authors
	// the CAPI-core objects in the v1beta2 contract klotho requires. VERIFY the
	// release and the contract compatibility against a live run.
	DefaultHetznerVersion = "v1.1.13"
	// ProviderHetzner is the hetzner (Hetzner Cloud) infrastructure provider's
	// name, as clusterctl and --provider know it.
	ProviderHetzner = "hetzner"
	// DefaultVSphereVersion is the upstream Cluster API provider for vSphere
	// (CAPV) release klotho installs for the vsphere provider. CAPV is in
	// clusterctl's built-in provider list, so no fetchURL is required for the
	// stock provider; klotho's flat single-node template is read from a local
	// override (providers.vsphere.fetchURL) since the released provider ships a
	// multi-node CPI/CSI template. The v1.x series exposes the govmomi-mode CRDs
	// as infrastructure.cluster.x-k8s.io/v1beta2. VERIFY against a live run.
	DefaultVSphereVersion = "v1.13.0"
	// ProviderVSphere is the vsphere infrastructure provider's name, as
	// clusterctl and --provider know it.
	ProviderVSphere = "vsphere"
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
	BackendVault   = "vault"
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
	// Backend is "proxmox", "file", "s3" or "vault". Empty means the
	// infrastructure provider's native store when it has one and is configured,
	// else file; see EffectiveBackend. "s3" and "vault" are never inferred and
	// must be set explicitly.
	Backend string `json:"backend,omitempty"`
	// Payload configures the host that serves files too large for a
	// provider's user-data channel. Unset means none.
	Payload Payload `json:"payload,omitempty"`
	// StateDir holds the file backend's records and the credential cache.
	StateDir string `json:"stateDir,omitempty"`
	// S3Backend configures the s3 state backend. It is used only when the
	// effective backend is "s3".
	S3Backend S3Backend `json:"s3,omitempty"`
	// VaultBackend configures the vault state backend. It is used only when the
	// effective backend is "vault".
	VaultBackend VaultBackend `json:"vault,omitempty"`
	// SSHAuthorizedKeys are installed for the bootstrap node's default user.
	SSHAuthorizedKeys []string     `json:"sshAuthorizedKeys,omitempty"`
	Encryption        Encryption   `json:"encryption,omitempty"`
	Proxmox           Proxmox      `json:"proxmox,omitempty"`
	AWS               AWS          `json:"aws,omitempty"`
	Linode            Linode       `json:"linode,omitempty"`
	GCP               GCP          `json:"gcp,omitempty"`
	OpenStack         OpenStack    `json:"openstack,omitempty"`
	Hetzner           Hetzner      `json:"hetzner,omitempty"`
	DigitalOcean      DigitalOcean `json:"digitalocean,omitempty"`
	Azure             Azure        `json:"azure,omitempty"`
	OCI               OCI          `json:"oci,omitempty"`
	Docker            Docker       `json:"docker,omitempty"`
	Hyperv            Hyperv       `json:"hyperv,omitempty"`
	Libvirt           Libvirt      `json:"libvirt,omitempty"`
	VZ                VZ           `json:"vz,omitempty"`
	VSphere           VSphere      `json:"vsphere,omitempty"`
	CAPI              CAPI         `json:"capi,omitempty"`
	// K0smotron configures the k0smotron bootstrap and control-plane providers
	// installed for --control-plane k0s. Empty fields use built-in defaults.
	K0smotron K0smotron `json:"k0smotron,omitempty"`
	// RKE2 configures the cluster-api-provider-rke2 bootstrap and control-plane
	// providers installed for --control-plane rke2. Empty fields use built-in
	// defaults.
	RKE2 RKE2 `json:"rke2,omitempty"`
	// K3s configures the cluster-api-k3s bootstrap and control-plane providers
	// installed for --control-plane k3s. Empty fields use built-in defaults.
	K3s K3s `json:"k3s,omitempty"`
	// Nodes configures what every node of a generated cluster does at boot.
	Nodes Nodes `json:"nodes,omitempty"`
	// Providers pins infrastructure provider releases by provider name.
	Providers map[string]Provider `json:"providers,omitempty"`
	// Variables are default template variables, below anything the user or
	// the provider sets.
	Variables map[string]string `json:"variables,omitempty"`
	// Hooks are the consumer's lifecycle hooks: their own actions to run
	// against the booted cluster at fixed moments (see Hook). klotho ships no
	// default hooks and installs nothing; a hook is entirely the consumer's.
	Hooks []Hook `json:"hooks,omitempty"`
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

// Hetzner holds the Hetzner Cloud connection and placement settings for the
// hetzner (CAPH) provider. The API token is not stored here; it comes from the
// environment (HCLOUD_TOKEN) and is never written back to disk.
type Hetzner struct {
	// Token is the Hetzner Cloud API token. It comes only from the environment
	// (HCLOUD_TOKEN), is never accepted from or written to the config file, and
	// is handed to the in-cluster CAPH controller as its credential.
	Token string `json:"-"`
	// Location is the Hetzner location the bootstrap server is placed in and the
	// cluster's control-plane region, e.g. "fsn1", "nbg1", "hel1", "ash", "hil",
	// "sin". Required for the hetzner provider.
	Location string `json:"location,omitempty"`
	// ServerType sizes the bootstrap server (and the template's machines); empty
	// means cpx21, the smallest type that clears kubeadm's two-CPU control-plane
	// preflight and is available in every location.
	ServerType string `json:"serverType,omitempty"`
	// Image is the bootstrap image name; empty means ubuntu-22.04 (a plain image,
	// so install-at-boot is needed).
	Image string `json:"image,omitempty"`
	// SSHKeys are the names of SSH keys that already exist in the Hetzner project.
	// At least one is required (CAPH requires an SSH key, and it is the only way
	// into a node to diagnose a failed bootstrap).
	SSHKeys []string `json:"sshKeys,omitempty"`
	// Labels are extra Hetzner labels added to the bootstrap server.
	Labels map[string]string `json:"labels,omitempty"`
	// ControlPlaneEndpoint is the API server host. Required: klotho mints
	// certificates and node zero's cloud-init before the node boots, and the flat
	// template disables CAPH's load balancer. The cheapest choice is a DNS name
	// you control, pointed at node zero after it boots.
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// Port is the API server port; zero means 6443.
	Port int `json:"port,omitempty"`
}

// DigitalOcean holds the DigitalOcean connection and placement settings for the
// digitalocean (CAPDO) provider. The API token is not stored here; it comes from
// the environment (DIGITALOCEAN_TOKEN) and is never written back to disk.
type DigitalOcean struct {
	// Token is the DigitalOcean API token. It comes only from the environment
	// (DIGITALOCEAN_TOKEN), is never accepted from or written to the config file,
	// and is handed to the in-cluster CAPDO controller as its credential.
	Token string `json:"-"`
	// Region is the DigitalOcean region slug, e.g. "nyc3". Required for the
	// digitalocean provider.
	Region string `json:"region,omitempty"`
	// Size sizes the bootstrap droplet (and the template's machines); empty means
	// s-2vcpu-4gb, the smallest size that clears kubeadm's two-CPU control-plane
	// preflight.
	Size string `json:"size,omitempty"`
	// Image is the bootstrap image slug or numeric ID; empty means
	// ubuntu-22-04-x64 (a plain image, so install-at-boot is needed).
	Image string `json:"image,omitempty"`
	// SSHKeys are DigitalOcean SSH key fingerprints or IDs (already in the
	// account) installed on the bootstrap droplet, so a failed bootstrap can be
	// diagnosed.
	SSHKeys []string `json:"sshKeys,omitempty"`
	// Tags are extra DigitalOcean tags added to the bootstrap droplet.
	Tags []string `json:"tags,omitempty"`
	// VPCUUID places the bootstrap droplet (and CAPDO's machines) in an existing
	// VPC; empty uses the region's default VPC.
	VPCUUID string `json:"vpcUuid,omitempty"`
	// LoadBalancerID reuses an existing DigitalOcean load balancer as the control
	// plane endpoint instead of creating one; empty means klotho creates one (an
	// unavoidable ~$12/mo standing cost, since stock CAPDO always fronts the API
	// server with a load balancer) so the endpoint is known before cert-minting.
	LoadBalancerID string `json:"loadBalancerId,omitempty"`
	// ControlPlaneEndpoint is the API server host. Empty means klotho creates (or
	// reuses, see LoadBalancerID) a load balancer and uses its IP. A value here is
	// used as-is (a DNS name you point at the load balancer).
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// Port is the API server port; zero means 6443.
	Port int `json:"port,omitempty"`
}

// OpenStack holds the OpenStack connection and placement settings for the
// openstack (CAPO) provider. Credentials live in a clouds.yaml (never inline in
// this config); its contents are handed, base64-encoded, to the in-cluster CAPO
// controller as a per-cluster credential Secret.
type OpenStack struct {
	// Cloud is the entry in clouds.yaml to authenticate with (also the cloudName
	// the in-cluster controller's identityRef selects). Required. Read from OS_CLOUD
	// when unset.
	Cloud string `json:"cloud,omitempty"`
	// CloudsYAML is an optional path to a clouds.yaml. Empty uses the standard
	// OpenStack locations (OS_CLIENT_CONFIG_FILE, ./clouds.yaml, ~/.config/openstack,
	// /etc/openstack).
	CloudsYAML string `json:"cloudsYaml,omitempty"`
	// Region overrides the clouds.yaml region when selecting service endpoints.
	Region string `json:"region,omitempty"`
	// Network is the name of the existing Neutron network node zero joins; the
	// cluster template references the same network so CAPO's machines land beside
	// it. Required.
	Network string `json:"network,omitempty"`
	// Subnet is the name of the subnet within Network; empty lets CAPO pick it.
	Subnet string `json:"subnet,omitempty"`
	// ExternalNetwork is the name of the external (public) network a control plane
	// floating IP is allocated from when ControlPlaneEndpoint is empty. Required.
	ExternalNetwork string `json:"externalNetwork,omitempty"`
	// Flavor is the Nova flavor name for the bootstrap server and the template's
	// machines. Required.
	Flavor string `json:"flavor,omitempty"`
	// Image is the Glance image name the bootstrap server boots from. Required; a
	// plain cloud image needs install-at-boot.
	Image string `json:"image,omitempty"`
	// SSHKeyName is an existing Nova keypair installed on the bootstrap server, so
	// a failed bootstrap can be diagnosed. Optional but strongly recommended.
	SSHKeyName string `json:"sshKeyName,omitempty"`
	// SecurityGroups are existing security group names applied to the bootstrap
	// server. Optional.
	SecurityGroups []string `json:"securityGroups,omitempty"`
	// AvailabilityZone places the bootstrap server. Optional.
	AvailabilityZone string `json:"availabilityZone,omitempty"`
	// ControlPlaneEndpoint is the API server host. Empty means klotho allocates a
	// Neutron floating IP from ExternalNetwork and associates it with node zero
	// (the cheapest endpoint, no Octavia load balancer). A value here (an existing
	// floating IP pointed at node zero, an Octavia VIP, or a DNS name) is used
	// as-is and no floating IP is allocated.
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// Port is the API server port; zero means 6443.
	Port int `json:"port,omitempty"`
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

// OCI holds the Oracle Cloud Infrastructure connection and placement settings
// for the oci (CAPOCI) provider. Credentials are not stored here: klotho
// authenticates with an OCI SDK config file (~/.oci/config) or the host's
// instance principal, and the in-cluster CAPOCI controller authenticates with
// its own OCIClusterIdentity / instance principal.
type OCI struct {
	// CompartmentID is the OCID of the compartment the cluster runs in. Required.
	CompartmentID string `json:"compartmentId,omitempty"`
	// Region is the OCI region identifier, e.g. "us-ashburn-1". Required.
	Region string `json:"region,omitempty"`
	// AvailabilityDomain is the AD the bootstrap instance is placed in, e.g.
	// "Uocm:US-ASHBURN-AD-1". Required.
	AvailabilityDomain string `json:"availabilityDomain,omitempty"`
	// VCNID and SubnetID are an existing VCN and subnet the bootstrap instance
	// joins; klotho points CAPOCI at the same ones so it adopts node zero in
	// place instead of creating a new VCN. Required (klotho creates no network).
	VCNID    string `json:"vcnId,omitempty"`
	SubnetID string `json:"subnetId,omitempty"`
	// NSGIDs are OCIDs of network security groups attached to the primary VNIC.
	NSGIDs []string `json:"nsgIds,omitempty"`
	// ImageID is the OCID of the boot image. Required: OCI images are
	// region/tenancy-specific OCIDs with no sane default. Use a plain Ubuntu
	// image (install-at-boot adds Kubernetes).
	ImageID string `json:"imageId,omitempty"`
	// Shape sizes the bootstrap instance (and the template's machines); empty
	// means VM.Standard.E4.Flex. For the Always Free tier use VM.Standard.A1.Flex
	// with an arm64 image.
	Shape string `json:"shape,omitempty"`
	// OCPUs and MemoryGBs size a flexible shape; zero means 2 OCPUs / 16 GB
	// (two OCPUs clear kubeadm's two-CPU control-plane preflight). Ignored for
	// fixed shapes.
	OCPUs     float32 `json:"ocpus,omitempty"`
	MemoryGBs float32 `json:"memoryGBs,omitempty"`
	// BootVolumeGBs sizes the boot volume; zero means 50 GB (OCI's minimum).
	BootVolumeGBs int64 `json:"bootVolumeGBs,omitempty"`
	// AssignPublicIP gives the bootstrap instance a public IP. Unset means true;
	// set it false only with a NAT gateway and another route to the endpoint.
	AssignPublicIP *bool `json:"assignPublicIp,omitempty"`
	// SSHAuthorizedKeys are public keys installed via the instance metadata, so a
	// failed bootstrap can be diagnosed.
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`
	// FreeformTags are extra OCI freeform tags added to the bootstrap instance.
	FreeformTags map[string]string `json:"freeformTags,omitempty"`
	// ResourceIdentifier is pinned into OCICluster.spec.ociResourceIdentifier and
	// the instance's ClusterResourceIdentifier freeform tag. Empty means the
	// cluster name, which the flat template also pins.
	ResourceIdentifier string `json:"resourceIdentifier,omitempty"`
	// ControlPlaneEndpoint is the API server host. Required: klotho mints the
	// certificates before the node boots, and stock CAPOCI learns its endpoint
	// only after provisioning a load balancer. Supply an address the control-plane
	// LB answers on, or node zero's own reserved public IP for a single node.
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// Port is the API server port; zero means 6443.
	Port int `json:"port,omitempty"`
	// ConfigFile is the path to an OCI SDK config file; empty means the SDK
	// default (~/.oci/config). Profile selects a profile; empty means DEFAULT.
	ConfigFile string `json:"configFile,omitempty"`
	Profile    string `json:"profile,omitempty"`
	// UseInstancePrincipal authenticates with the instance principal of the host
	// klotho runs on instead of a config file; takes precedence over ConfigFile.
	UseInstancePrincipal bool `json:"useInstancePrincipal,omitempty"`
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

// Hyperv holds the Microsoft Hyper-V provider settings for local development
// clusters. klotho runs on the Windows host and shells to PowerShell. The
// Cluster API Hyper-V provider is development-oriented, not for production.
type Hyperv struct {
	// Switch is the virtual switch the bootstrap VM joins. Empty means the
	// HYPERV_SWITCH_NAME variable, then "Default Switch".
	Switch string `json:"switch,omitempty"`
	// Image is the path to the cloud-init-ready base Linux VHDX the bootstrap
	// VM's disk is copied from. Empty means the HYPERV_IMAGE_PATH variable.
	Image string `json:"image,omitempty"`
	// VMPath is the directory VM disks are written under. Empty means the
	// host default.
	VMPath string `json:"vmPath,omitempty"`
	// ISODir is where the NoCloud cidata ISO is written. Empty means VMPath.
	ISODir string `json:"isoDir,omitempty"`
	// MemoryMiB and CPUs size the bootstrap VM. Zero means 4096 and 2.
	MemoryMiB int `json:"memoryMiB,omitempty"`
	CPUs      int `json:"cpus,omitempty"`
	// EndpointHost is the static control plane endpoint address; Hyper-V has
	// no load balancer. Empty means the CONTROL_PLANE_ENDPOINT_HOST variable.
	EndpointHost string `json:"endpointHost,omitempty"`
	// SecureBoot enables UEFI Secure Boot. Default false, since stock Linux
	// VHDX images are not signed for the Microsoft UEFI CA.
	SecureBoot bool `json:"secureBoot,omitempty"`

	// --- in-cluster Hyper-V agent (gRPC daemon) settings ---
	//
	// klotho boots node zero host-side over local PowerShell (the fields
	// above). The in-cluster Cluster API Hyper-V controller cannot shell to
	// the host, so it reaches a gRPC agent (hyperv-agent) running on the
	// Windows host over the network. These fields are handed to that
	// controller through the operator's configSecret as HYPERV_AGENT_ENDPOINT,
	// HYPERV_AGENT_CA and HYPERV_AGENT_TOKEN. On Windows, "klotho create"
	// detects a running hyperv-agent service and fills any of these left empty.

	// AgentEndpoint is the agent's "host:port" (for example
	// "172.30.32.1:7443"). Empty means the HYPERV_AGENT_ENDPOINT variable.
	AgentEndpoint string `json:"agentEndpoint,omitempty"`
	// AgentCA is the agent's CA certificate as inline PEM; it wins over
	// AgentCAFile. Empty means read AgentCAFile.
	AgentCA string `json:"agentCa,omitempty"`
	// AgentCAFile is the path to the agent's CA certificate on this host (for
	// example C:\ProgramData\hyperv-agent\agent.crt).
	AgentCAFile string `json:"agentCaFile,omitempty"`
	// AgentToken is the bearer token the controller presents; it wins over
	// AgentTokenFile. Empty means read AgentTokenFile.
	AgentToken string `json:"agentToken,omitempty"`
	// AgentTokenFile is the path to a file holding the agent token.
	AgentTokenFile string `json:"agentTokenFile,omitempty"`
	// AgentBinary is a path or URL the agent executable is sourced from when
	// "klotho create" or "klotho hyperv-agent install" installs the service.
	// Empty means download the provider's release asset.
	AgentBinary string `json:"agentBinary,omitempty"`
}

// ResolvedAgentEndpoint returns the agent endpoint from config, then the
// HYPERV_AGENT_ENDPOINT environment variable.
func (h Hyperv) ResolvedAgentEndpoint() string {
	if h.AgentEndpoint != "" {
		return h.AgentEndpoint
	}
	return os.Getenv("HYPERV_AGENT_ENDPOINT")
}

// ResolvedAgentCA returns the agent CA PEM: inline AgentCA, then the contents
// of AgentCAFile, then the HYPERV_AGENT_CA environment variable.
func (h Hyperv) ResolvedAgentCA() (string, error) {
	if h.AgentCA != "" {
		return h.AgentCA, nil
	}
	if h.AgentCAFile != "" {
		b, err := os.ReadFile(Expand(h.AgentCAFile))
		if err != nil {
			return "", fmt.Errorf("hyperv.agentCaFile: %w", err)
		}
		return string(b), nil
	}
	return os.Getenv("HYPERV_AGENT_CA"), nil
}

// ResolvedAgentToken returns the agent token: inline AgentToken, then the
// contents of AgentTokenFile, then the HYPERV_AGENT_TOKEN environment variable.
func (h Hyperv) ResolvedAgentToken() (string, error) {
	if h.AgentToken != "" {
		return h.AgentToken, nil
	}
	if h.AgentTokenFile != "" {
		b, err := os.ReadFile(Expand(h.AgentTokenFile))
		if err != nil {
			return "", fmt.Errorf("hyperv.agentTokenFile: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv("HYPERV_AGENT_TOKEN"), nil
}

// Libvirt holds the libvirt/KVM connection and placement settings for the
// libvirt (Cluster API libvirt) provider. klotho reaches libvirtd directly over
// the connection URI; TLS/SSH credentials for a remote URI are the controller's
// concern (a secretRef on the LibvirtCluster), not stored here.
type Libvirt struct {
	// URI is the libvirt connection URI klotho the CLI dials, for example
	// "qemu:///system" (a local socket), "qemu+tcp://host/system" or
	// "qemu+tls://host/system". Empty means the LIBVIRT_URI variable, then
	// "qemu:///system". It comes from the environment (KLOTHO_LIBVIRT_URI) too.
	URI string `json:"uri,omitempty"`
	// ControllerURI is the connection URI the in-cluster Cluster API libvirt
	// controller uses to reach the same libvirtd. The controller runs in a pod,
	// so this MUST be a network URI it can reach (qemu+tcp:// or qemu+tls://); a
	// local-socket URI such as "qemu:///system" is unreachable from a pod and
	// create blocks on it. Empty falls back to URI, which then must itself be a
	// network URI. This lets klotho the CLI dial a fast local socket (uri) while
	// the controller uses a TCP/TLS URI - for example on a WSL2 host, uri:
	// qemu:///system and controllerURI: qemu+tcp://<bridge-gateway>/system.
	ControllerURI string `json:"controllerURI,omitempty"`
	// StoragePool holds the base image and receives the per-domain overlay and
	// cidata seed. Empty means the LIBVIRT_POOL variable, then "default".
	StoragePool string `json:"storagePool,omitempty"`
	// Image is the base cloud-image qcow2 volume name in StoragePool the
	// bootstrap overlay is backed by. Empty means the LIBVIRT_IMAGE variable.
	// Required; the provider never downloads or builds it.
	Image string `json:"image,omitempty"`
	// Network is the libvirt virtual network (NetworkType "network") or host
	// bridge (NetworkType "bridge") the NIC attaches to. Empty means the
	// LIBVIRT_NETWORK variable, then "default".
	Network string `json:"network,omitempty"`
	// NetworkType is "network" (a libvirt virtual network, the default) or
	// "bridge" (a host bridge).
	NetworkType string `json:"networkType,omitempty"`
	// MemoryMiB and CPUs size the bootstrap domain. Zero means 4096 and 2.
	MemoryMiB int `json:"memoryMiB,omitempty"`
	CPUs      int `json:"cpus,omitempty"`
	// DiskGiB optionally grows the overlay's virtual capacity. Zero inherits
	// the base image's virtual size.
	DiskGiB int `json:"diskGiB,omitempty"`
	// EndpointHost is the static control plane endpoint address; libvirt has no
	// load balancer. Empty means the CONTROL_PLANE_ENDPOINT_HOST variable.
	EndpointHost string `json:"endpointHost,omitempty"`
}

// VZ holds the connection and VM settings for the vz (Apple
// Virtualization.framework) provider. klotho reaches the host-side capvz-daemon
// over gRPC with TLS and a bearer token; the in-cluster Cluster API vz controller
// reaches the same daemon, so the endpoint and token are handed to it as a
// Secret.
type VZ struct {
	// Endpoint is the capvz-daemon's gRPC address klotho dials, "host:port" (for
	// example "my-mac.local:7443"). Required for any operation that reaches the
	// daemon.
	Endpoint string `json:"endpoint,omitempty"`
	// CA is the path to a PEM CA bundle that signs the daemon's TLS certificate.
	// Empty means the host's system root CAs; point it at the cert the daemon
	// wrote to its data dir on first run when it is self-signed.
	CA string `json:"ca,omitempty"`
	// ServerName overrides the TLS server name verified against the daemon's
	// certificate. Empty means the Endpoint host. Set it when dialing by IP a
	// certificate issued for a hostname.
	ServerName string `json:"serverName,omitempty"`
	// Token is the bearer token every gRPC call presents. It comes from the
	// environment (KLOTHO_VZ_TOKEN) too, and is handed to the in-cluster
	// controller as VZ_DAEMON_TOKEN. klotho never writes it back to disk.
	Token string `json:"token,omitempty"`
	// Host is the vz host recorded in machine provider IDs (vz://<host>/<vmid>).
	// Empty means the VZ_HOST variable, then "localhost". It must match the
	// kubelet --provider-id host.
	Host string `json:"host,omitempty"`
	// Image is the path, on the Mac host, to the raw arm64 cloud-init enabled
	// base disk image the daemon copies for the bootstrap VM. Empty means the
	// VZ_IMAGE_PATH variable. Required; the provider never downloads or builds it.
	Image string `json:"image,omitempty"`
	// MemoryMiB and CPUs size the bootstrap VM. Zero means 4096 and 2.
	MemoryMiB int `json:"memoryMiB,omitempty"`
	CPUs      int `json:"cpus,omitempty"`
	// EndpointHost is the static control plane endpoint address; vz has no load
	// balancer. Empty means the CONTROL_PLANE_ENDPOINT_HOST variable.
	EndpointHost string `json:"endpointHost,omitempty"`
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

// VSphere holds the vCenter connection and placement settings for the vsphere
// (Cluster API provider for vSphere, CAPV) provider. klotho reaches vCenter
// directly over the govmomi SDK; the in-cluster controller uses a per-cluster
// identity Secret built from username/password.
type VSphere struct {
	// Server is the vCenter host or URL klotho dials, for example
	// "vcenter.example.com" or "https://vcenter.example.com/sdk". Empty means the
	// VSPHERE_SERVER variable. Required.
	Server string `json:"server,omitempty"`
	// Username and Password authenticate the govmomi session and the in-cluster
	// identity Secret. They come from the environment (VSPHERE_USERNAME,
	// VSPHERE_PASSWORD) too; the password is never written back to disk.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Thumbprint is the colon-separated SHA-1 of the vCenter host certificate
	// (VSPHERE_TLS_THUMBPRINT). Empty with Insecure true skips verification.
	Thumbprint string `json:"thumbprint,omitempty"`
	// Insecure skips vCenter TLS verification for klotho's own session.
	Insecure bool `json:"insecure,omitempty"`
	// Datacenter, Datastore, Folder, ResourcePool and Network place the clone.
	// Empty fields fall back to the matching VSPHERE_* variable, then the
	// datacenter defaults.
	Datacenter   string `json:"datacenter,omitempty"`
	Datastore    string `json:"datastore,omitempty"`
	Folder       string `json:"folder,omitempty"`
	ResourcePool string `json:"resourcePool,omitempty"`
	Network      string `json:"network,omitempty"`
	// Template is the name (or inventory path) of the VM template to clone. Empty
	// means the VSPHERE_TEMPLATE variable. Required.
	Template string `json:"template,omitempty"`
	// CloneMode is "fullClone" (the default) or "linkedClone".
	CloneMode string `json:"cloneMode,omitempty"`
	// NumCPUs, MemoryMiB and DiskGiB size the bootstrap VM. Zero means 2, 8192
	// and the template's disk size.
	NumCPUs   int `json:"numCPUs,omitempty"`
	MemoryMiB int `json:"memoryMiB,omitempty"`
	DiskGiB   int `json:"diskGiB,omitempty"`
	// EndpointIP is the static control plane VIP kube-vip holds (vSphere has no
	// load balancer). Empty means the CONTROL_PLANE_ENDPOINT_IP variable.
	EndpointIP string `json:"endpointIP,omitempty"`
	// Port is the API server port. Zero means 6443.
	Port int `json:"port,omitempty"`
	// VIPInterface names the NIC kube-vip binds the VIP on. Empty lets kube-vip
	// auto-detect.
	VIPInterface string `json:"vipInterface,omitempty"`
	// SSHAuthorizedKey is installed for the template's default user so a failed
	// bootstrap can be diagnosed (VSPHERE_SSH_AUTHORIZED_KEY). Optional.
	SSHAuthorizedKey string `json:"sshAuthorizedKey,omitempty"`
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

// VaultBackend configures the vault state backend: a HashiCorp Vault KV v2
// secrets engine. The backend stores each cluster's already-sealed state record
// as one secret; it never holds a plaintext state or an encryption key. The
// Vault token is not stored here — it comes from the environment (VAULT_TOKEN)
// and is never written back to disk.
type VaultBackend struct {
	// Address is the Vault server URL, e.g. https://vault.example:8200. Empty
	// means the VAULT_ADDR environment variable (then the SDK default).
	Address string `json:"address,omitempty"`
	// Token is the Vault token. It comes only from the environment (VAULT_TOKEN),
	// is never accepted from or written to the config file.
	Token string `json:"-"`
	// Mount is the path the KV v2 engine is mounted at; empty means "secret".
	Mount string `json:"mount,omitempty"`
	// Prefix is a path prefix below the mount under which every record is stored.
	Prefix string `json:"prefix,omitempty"`
	// Namespace is the Vault Enterprise / HCP namespace, if any. It comes from
	// VAULT_NAMESPACE too.
	Namespace string `json:"namespace,omitempty"`
	// Insecure skips TLS verification when dialing Vault. Development only.
	Insecure bool `json:"insecure,omitempty"`
}

// problems lists what is wrong with a vault backend section.
func (v VaultBackend) problems() []string {
	var problems []string
	addr := v.Address
	if addr == "" {
		addr = os.Getenv(EnvVaultAddr)
	}
	if addr == "" {
		problems = append(problems, fmt.Sprintf("vault.address is required when backend is vault (or set %s)", EnvVaultAddr))
	} else if u, err := url.Parse(addr); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		problems = append(problems, fmt.Sprintf("vault.address %q must be an http(s) URL", addr))
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
	// Images overrides the controller image of the core, bootstrap and control
	// plane providers without rewriting the fetched component manifests. Empty
	// fields change nothing.
	Images CAPIImages `json:"images,omitempty"`
}

// CAPIImages overrides the manager container image of the Cluster API core,
// bootstrap and control plane providers. The Cluster API operator keeps the
// image repository it knows for a provider and only follows the version tag, so
// an image here is how the operator is pointed at another repository's build
// (a fork, or a public mirror) without touching the fetched components. The
// motivating case is core: when the default core controller image lives in a
// private registry, pointing Core at a public one (for example
// ghcr.io/klotho-k8s/cluster-api-controller) lets the self-managed cluster pull
// it. Empty fields leave the provider's own image untouched.
type CAPIImages struct {
	// Core overrides the core provider (cluster-api) manager image.
	Core string `json:"core,omitempty"`
	// Bootstrap overrides the bootstrap provider manager image.
	Bootstrap string `json:"bootstrap,omitempty"`
	// ControlPlane overrides the control plane provider manager image.
	ControlPlane string `json:"controlPlane,omitempty"`
}

// K0smotron pins the k0smotron (k0s) bootstrap and control-plane provider
// release the Cluster API operator installs for --control-plane k0s. Empty
// fields fall back to the k0s provider's built-in defaults.
type K0smotron struct {
	// Version is the k0smotron release, e.g. v2.3.1.
	Version string `json:"version,omitempty"`
	// Namespace is where the k0smotron providers run (default k0smotron).
	Namespace string `json:"namespace,omitempty"`
	// BootstrapURL and ControlPlaneURL override the components manifests the
	// in-cluster operator fetches. Empty derives them from the release assets
	// for Version.
	BootstrapURL    string `json:"bootstrapURL,omitempty"`
	ControlPlaneURL string `json:"controlPlaneURL,omitempty"`
}

// RKE2 pins the cluster-api-provider-rke2 bootstrap and control-plane provider
// release the Cluster API operator installs for --control-plane rke2. Empty
// fields fall back to the rke2 provider's built-in defaults.
type RKE2 struct {
	// Version is the cluster-api-provider-rke2 release, e.g. v0.27.3.
	Version string `json:"version,omitempty"`
	// BootstrapNamespace and ControlPlaneNamespace are where the RKE2 bootstrap
	// and control-plane providers run (defaults rke2-bootstrap-system and
	// rke2-control-plane-system).
	BootstrapNamespace    string `json:"bootstrapNamespace,omitempty"`
	ControlPlaneNamespace string `json:"controlPlaneNamespace,omitempty"`
	// BootstrapURL and ControlPlaneURL override the components manifests the
	// in-cluster operator fetches. Empty derives them from the release assets
	// for Version.
	BootstrapURL    string `json:"bootstrapURL,omitempty"`
	ControlPlaneURL string `json:"controlPlaneURL,omitempty"`
}

// K3s pins the cluster-api-k3s bootstrap and control-plane provider release the
// Cluster API operator installs for --control-plane k3s. Empty fields fall back
// to the k3s provider's built-in defaults.
type K3s struct {
	// Version is the cluster-api-k3s release, e.g. v0.5.1.
	Version string `json:"version,omitempty"`
	// BootstrapNamespace and ControlPlaneNamespace are where the k3s bootstrap
	// and control-plane providers run (defaults capi-k3s-bootstrap-system and
	// capi-k3s-control-plane-system).
	BootstrapNamespace    string `json:"bootstrapNamespace,omitempty"`
	ControlPlaneNamespace string `json:"controlPlaneNamespace,omitempty"`
	// BootstrapURL and ControlPlaneURL override the components manifests the
	// in-cluster operator fetches. Empty derives them from the release assets
	// for Version.
	BootstrapURL    string `json:"bootstrapURL,omitempty"`
	ControlPlaneURL string `json:"controlPlaneURL,omitempty"`
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

// Hook is one lifecycle hook from the configuration: an action the consumer
// runs against the booted cluster at a fixed moment. klotho validates and
// dispatches it but has no opinion about what it does.
type Hook struct {
	// Name identifies the hook in logs and errors. Required.
	Name string `json:"name"`
	// Event is when the hook fires: postPivot (the cluster is self-managed and
	// healthy) or preDelete (before teardown). Required. See hooks.Events.
	Event string `json:"event"`
	// Type is how klotho runs the hook: exec (run a command) or apply (apply a
	// manifest). Empty means exec.
	Type string `json:"type,omitempty"`
	// Command and Args are the exec command line (type exec).
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Manifest is an apply source: a file, a directory of YAML, or an http(s)
	// URL (type apply). An apply hook sets manifest or helm, not both.
	Manifest string `json:"manifest,omitempty"`
	// Helm is an apply source: a Helm release to install or upgrade (type
	// apply). An apply hook sets manifest or helm, not both.
	Helm *HelmHook `json:"helm,omitempty"`
	// Timeout bounds the hook, as a Go duration such as 10m. Empty means no
	// klotho-imposed bound.
	Timeout string `json:"timeout,omitempty"`
	// OnFailure is fail (abort the operation; the default) or warn (log and
	// continue).
	OnFailure string `json:"onFailure,omitempty"`
}

// HelmHook is the helm sub-object of an apply hook: a Helm release klotho
// installs or upgrades against the booted cluster.
type HelmHook struct {
	// Chart is the chart reference: a repository path (such as "stable/nginx",
	// which requires a configured repository), an "oci://" reference, a
	// packaged-chart URL, or a local path. Required.
	Chart string `json:"chart"`
	// Version is the chart version. Empty means the latest found.
	Version string `json:"version,omitempty"`
	// Namespace is the release namespace. Empty means "default"; klotho creates
	// it if it does not exist.
	Namespace string `json:"namespace,omitempty"`
	// ReleaseName names the release. Empty derives it from the chart name.
	ReleaseName string `json:"releaseName,omitempty"`
	// Values is either a local values-file path (a string) or an inline values
	// mapping (an object). Optional.
	Values json.RawMessage `json:"values,omitempty"`
}

// HookSpecs converts the configured hooks into the resolved form the library
// runs, parsing timeouts and defaulting the type to exec and the policy to
// fail. Load has already validated the values.
func (c *Config) HookSpecs() ([]hooks.Hook, error) {
	if len(c.Hooks) == 0 {
		return nil, nil
	}
	out := make([]hooks.Hook, 0, len(c.Hooks))
	for _, h := range c.Hooks {
		typ := hooks.Type(h.Type)
		if typ == "" {
			typ = hooks.Exec
		}
		policy := hooks.Policy(h.OnFailure)
		if policy == "" {
			policy = hooks.Fail
		}
		var timeout time.Duration
		if h.Timeout != "" {
			d, err := time.ParseDuration(h.Timeout)
			if err != nil {
				return nil, fmt.Errorf("config: hook %q: timeout %q: %w", h.Name, h.Timeout, err)
			}
			timeout = d
		}
		helmSpec, err := h.helmSpec()
		if err != nil {
			return nil, err
		}
		out = append(out, hooks.Hook{
			Name:      h.Name,
			Event:     hooks.Event(h.Event),
			Type:      typ,
			Command:   h.Command,
			Args:      h.Args,
			Manifest:  h.Manifest,
			Helm:      helmSpec,
			Timeout:   timeout,
			OnFailure: policy,
		})
	}
	return out, nil
}

// helmSpec resolves the hook's helm sub-object into the library form, parsing
// the polymorphic values field: a JSON string is a local values-file path, a
// JSON object is an inline values mapping.
func (h Hook) helmSpec() (*hooks.HelmSpec, error) {
	if h.Helm == nil {
		return nil, nil
	}
	spec := &hooks.HelmSpec{
		Chart:       h.Helm.Chart,
		Version:     h.Helm.Version,
		Namespace:   h.Helm.Namespace,
		ReleaseName: h.Helm.ReleaseName,
	}
	raw := bytes.TrimSpace(h.Helm.Values)
	if len(raw) == 0 || string(raw) == "null" {
		return spec, nil
	}
	switch raw[0] {
	case '"':
		var file string
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, fmt.Errorf("config: hook %q: helm values: %w", h.Name, err)
		}
		spec.ValuesFile = file
	case '{':
		var vals map[string]any
		if err := json.Unmarshal(raw, &vals); err != nil {
			return nil, fmt.Errorf("config: hook %q: helm values: %w", h.Name, err)
		}
		spec.Values = vals
	default:
		return nil, fmt.Errorf("config: hook %q: helm values must be a file path (string) or an inline mapping", h.Name)
	}
	return spec, nil
}

// problems lists what is wrong with a hook.
func (h Hook) problems() []string {
	var problems []string
	label := "hooks"
	if h.Name != "" {
		label = fmt.Sprintf("hook %q", h.Name)
	} else {
		problems = append(problems, "a hook needs a name")
	}
	switch {
	case h.Event == "":
		problems = append(problems, label+": event is required")
	case !hooks.Event(h.Event).Valid():
		problems = append(problems, fmt.Sprintf("%s: event %q is not one of %s", label, h.Event, hookEventList()))
	}
	typ := hooks.Type(h.Type)
	switch {
	case h.Type == "" || typ == hooks.Exec:
		if h.Command == "" {
			problems = append(problems, label+": an exec hook needs a command")
		}
		if h.Manifest != "" {
			problems = append(problems, label+": an exec hook may not set manifest")
		}
		if h.Helm != nil {
			problems = append(problems, label+": an exec hook may not set helm")
		}
	case typ == hooks.Apply:
		hasManifest := h.Manifest != ""
		hasHelm := h.Helm != nil
		switch {
		case !hasManifest && !hasHelm:
			problems = append(problems, label+": an apply hook needs a manifest or helm")
		case hasManifest && hasHelm:
			problems = append(problems, label+": an apply hook sets exactly one of manifest or helm")
		}
		if hasHelm && h.Helm.Chart == "" {
			problems = append(problems, label+": an apply hook's helm needs a chart")
		}
		if h.Command != "" || len(h.Args) > 0 {
			problems = append(problems, label+": an apply hook may not set command or args")
		}
	default:
		problems = append(problems, fmt.Sprintf("%s: type %q is not one of %s", label, h.Type, hookTypeList()))
	}
	if !hooks.Policy(h.OnFailure).Valid() {
		problems = append(problems, fmt.Sprintf("%s: onFailure %q is not one of %s, %s", label, h.OnFailure, hooks.Fail, hooks.Warn))
	}
	if h.Timeout != "" {
		if d, err := time.ParseDuration(h.Timeout); err != nil || d <= 0 {
			problems = append(problems, fmt.Sprintf("%s: timeout %q must be a positive duration such as 10m", label, h.Timeout))
		}
	}
	return problems
}

func hookEventList() string {
	parts := make([]string, 0, len(hooks.Events()))
	for _, e := range hooks.Events() {
		parts = append(parts, string(e))
	}
	return strings.Join(parts, ", ")
}

func hookTypeList() string {
	parts := make([]string, 0, len(hooks.Types()))
	for _, t := range hooks.Types() {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ", ")
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
	if v, ok := os.LookupEnv(EnvVaultToken); ok && v != "" {
		c.VaultBackend.Token = v
	}
	if c.VaultBackend.Address == "" {
		if v, ok := os.LookupEnv(EnvVaultAddr); ok && v != "" {
			c.VaultBackend.Address = v
		}
	}
	if c.VaultBackend.Namespace == "" {
		if v, ok := os.LookupEnv(EnvVaultNamespace); ok && v != "" {
			c.VaultBackend.Namespace = v
		}
	}
	if v, ok := os.LookupEnv(EnvLinodeToken); ok && v != "" {
		c.Linode.Token = v
	}
	if v, ok := os.LookupEnv(EnvHetznerToken); ok && v != "" {
		c.Hetzner.Token = v
	}
	if c.DigitalOcean.Token == "" {
		if v, ok := os.LookupEnv(EnvDOToken); ok && v != "" {
			c.DigitalOcean.Token = v
		}
	}
	if c.GCP.CredentialsFile == "" {
		if v, ok := os.LookupEnv(EnvGCPCredentials); ok && v != "" {
			c.GCP.CredentialsFile = v
		}
	}
	if c.OpenStack.Cloud == "" {
		// OS_CLOUD is the standard OpenStack environment selector for a clouds.yaml
		// entry.
		if v, ok := os.LookupEnv("OS_CLOUD"); ok && v != "" {
			c.OpenStack.Cloud = v
		}
	}
	if v, ok := os.LookupEnv(EnvAzureClientSecret); ok && v != "" {
		c.Azure.ClientSecret = v
	}
	if c.Libvirt.URI == "" {
		if v, ok := os.LookupEnv(EnvLibvirtURI); ok && v != "" {
			c.Libvirt.URI = v
		}
	}
	if c.VZ.Token == "" {
		if v, ok := os.LookupEnv(EnvVZToken); ok && v != "" {
			c.VZ.Token = v
		}
	}
	if c.VSphere.Username == "" {
		if v, ok := os.LookupEnv(EnvVSphereUsername); ok && v != "" {
			c.VSphere.Username = v
		}
	}
	if c.VSphere.Password == "" {
		if v, ok := os.LookupEnv(EnvVSpherePassword); ok && v != "" {
			c.VSphere.Password = v
		}
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
	hetzner := c.Providers[ProviderHetzner]
	if hetzner.Version == "" {
		hetzner.Version = DefaultHetznerVersion
	}
	c.Providers[ProviderHetzner] = hetzner
	do := c.Providers[ProviderDO]
	if do.Version == "" {
		do.Version = DefaultDOVersion
	}
	c.Providers[ProviderDO] = do
	openstack := c.Providers[ProviderOpenStack]
	if openstack.Version == "" {
		openstack.Version = DefaultOpenStackVersion
	}
	c.Providers[ProviderOpenStack] = openstack
	c.OpenStack.CloudsYAML = Expand(c.OpenStack.CloudsYAML)
	azure := c.Providers[ProviderAzure]
	if azure.Version == "" {
		azure.Version = DefaultAzureVersion
	}
	c.Providers[ProviderAzure] = azure
	oci := c.Providers[ProviderOCI]
	if oci.Version == "" {
		oci.Version = DefaultOCIVersion
	}
	c.Providers[ProviderOCI] = oci
	c.OCI.ConfigFile = Expand(c.OCI.ConfigFile)
	hv := c.Providers[ProviderHyperV]
	if hv.Version == "" {
		hv.Version = DefaultHyperVVersion
	}
	c.Providers[ProviderHyperV] = hv
	c.Hyperv.AgentCAFile = Expand(c.Hyperv.AgentCAFile)
	c.Hyperv.AgentTokenFile = Expand(c.Hyperv.AgentTokenFile)
	libvirt := c.Providers[ProviderLibvirt]
	if libvirt.Version == "" {
		libvirt.Version = DefaultLibvirtVersion
	}
	c.Providers[ProviderLibvirt] = libvirt
	vzp := c.Providers[ProviderVZ]
	if vzp.Version == "" {
		vzp.Version = DefaultVZVersion
	}
	c.Providers[ProviderVZ] = vzp
	vsp := c.Providers[ProviderVSphere]
	if vsp.Version == "" {
		vsp.Version = DefaultVSphereVersion
	}
	c.Providers[ProviderVSphere] = vsp
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
	case BackendVault:
		problems = append(problems, c.VaultBackend.problems()...)
	default:
		problems = append(problems, fmt.Sprintf("backend %q is not one of %s, %s, %s, %s", c.Backend, BackendProxmox, BackendFile, BackendS3, BackendVault))
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
	for _, h := range c.Hooks {
		problems = append(problems, h.problems()...)
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
#   version: v1.12.9        # the core, bootstrap and control plane provider line
#   images:                 # override a controller image without rewriting components;
#                           # the operator keeps the version tag but follows these repos
#     core: ghcr.io/klotho-k8s/cluster-api-controller:v1.12.9  # e.g. a public mirror of a
#                           # private default core image, so the self-managed cluster can pull it
#     # bootstrap: ghcr.io/<owner>/cluster-api-kubeadm-bootstrap-controller:v1.12.9
#     # controlPlane: ghcr.io/<owner>/cluster-api-kubeadm-control-plane-controller:v1.12.9
# libvirt:
#   uri: qemu:///system      # klotho the CLI's URI; or qemu+tcp://host/system, qemu+tls://host/system
#   # uri: set KLOTHO_LIBVIRT_URI instead of storing it here
#   controllerURI: qemu+tcp://192.168.122.1/system  # the in-pod controller needs a network URI,
#                            # not a local socket; empty falls back to uri (which must then be a network URI)
#   storagePool: default
#   image: ubuntu-24.qcow2   # a cloud-init qcow2 volume in the pool (noble; debian-12 generic lacks cloud-init)
#   network: default         # a libvirt NAT network
#   endpointHost: 192.168.122.10           # node zero's address (no load balancer)
# vz:                          # Apple Virtualization.framework on an Apple-Silicon Mac
#   endpoint: my-mac.local:7443   # the capvz-daemon's gRPC address (host:port)
#   # token: set KLOTHO_VZ_TOKEN instead of storing it here
#   ca: ~/.config/klotho/capvz-ca.pem   # omit to trust the system roots
#   host: localhost            # recorded in providerID vz://<host>/<vmid>
#   image: /opt/images/ubuntu-24.04-arm64.raw   # a raw arm64 cloud image on the Mac
#   endpointHost: 192.168.64.10            # node zero's address (no load balancer)
# vsphere:                      # VMware vCenter (Cluster API provider for vSphere, CAPV)
#   server: vcenter.example.com   # or https://vcenter.example.com/sdk
#   # username: set VSPHERE_USERNAME; password: set VSPHERE_PASSWORD (never store it here)
#   thumbprint: "AA:BB:CC:..."    # SHA-1 of the vCenter cert; or insecure: true (dev only)
#   datacenter: dc1
#   datastore: datastore1
#   folder: klotho               # a VM folder; empty = datacenter default
#   resourcePool: "*/Resources"  # empty = datacenter default
#   network: "VM Network"
#   template: ubuntu-2404-template   # a cloud-init + VMware guestinfo enabled VM template
#   endpointIP: 192.168.1.200    # the static control-plane VIP kube-vip holds (no load balancer)
# providers:
#   proxmox:
#     version: v0.9.2
#   libvirt:
#     version: v0.1.0
#     # libvirt is not in clusterctl's built-in list, so point these at its release:
#     # fetchURL: https://github.com/klotho-k8s/cluster-api-provider-libvirt/releases/<version>/infrastructure-components.yaml
#     # a release of another repository (a fork): its components URL and, because the
#     # operator keeps the image repository it knows, the manager image to run
#     # fetchURL: https://github.com/<owner>/cluster-api-provider-proxmox/releases/<version>/infrastructure-components.yaml
#     # image: ghcr.io/<owner>/cluster-api-provider-proxmox:<version>
#   vz:
#     version: v0.1.0
#     # vz is not in clusterctl's built-in list, so point these at its release:
#     # fetchURL: https://github.com/klotho-k8s/cluster-api-provider-vz/releases/<version>/infrastructure-components.yaml
#   vsphere:
#     version: v1.13.0
#     # CAPV is in clusterctl's built-in list, but its released template is multi-node CPI/CSI.
#     # Point fetchURL at a local clusterctl repo carrying klotho's flat single-node template,
#     # and componentsURL at the real CAPV release the in-cluster operator installs:
#     # fetchURL: file:///path/to/local-capv-repo/infrastructure-vsphere/v1.13.0/infrastructure-components.yaml
#     # componentsURL: https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/releases/download/v1.13.0/infrastructure-components.yaml
# variables:
#   CONTROL_PLANE_ENDPOINT_IP: 10.0.0.10
# hooks:                       # your own actions against the booted cluster; klotho installs nothing
#   - name: flux-bootstrap     # identifies the hook in logs
#     event: postPivot         # postPivot (cluster is self-managed) | preDelete (before teardown)
#     type: exec               # exec (run a command) | apply (apply a manifest or install a helm chart)
#     command: flux            # an exec hook runs with KUBECONFIG and KLOTHO_CLUSTER* set
#     args: [bootstrap, github, --owner=me, --repository=fleet, --path=clusters/lab]
#     timeout: 10m             # optional Go duration; empty means unbounded
#     onFailure: fail          # fail (abort; default) | warn (log and continue)
#   - name: cni                # an apply hook applies a manifest to the booted cluster
#     event: postPivot
#     type: apply
#     manifest: ./manifests/   # a file, a directory of YAML, or an http(s) URL
#     timeout: 5m
#   - name: ingress            # an apply hook can instead install or upgrade a helm release
#     event: postPivot
#     type: apply
#     helm:
#       chart: oci://ghcr.io/nginx/charts/nginx-ingress  # repo path, oci:// ref, URL, or local path
#       version: 2.3.0         # optional; empty means the latest
#       namespace: ingress     # created if absent; empty means default
#       releaseName: ingress   # optional; empty derives from the chart name
#       values: ./values.yaml  # a local values file path, or an inline mapping
#     onFailure: fail
#   - name: remove-apps
#     event: preDelete
#     type: exec
#     command: ./teardown.sh
#     onFailure: warn
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
