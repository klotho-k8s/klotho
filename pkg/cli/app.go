package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	gp "github.com/luthermonson/go-proxmox"
	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/config"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
	azureblobbackend "github.com/klotho-k8s/klotho/provider/backend/azureblob"
	filebackend "github.com/klotho-k8s/klotho/provider/backend/file"
	proxmoxbackend "github.com/klotho-k8s/klotho/provider/backend/proxmox"
	s3backend "github.com/klotho-k8s/klotho/provider/backend/s3"
	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/controlplane/k0s"
	"github.com/klotho-k8s/klotho/provider/controlplane/k3s"
	"github.com/klotho-k8s/klotho/provider/controlplane/kubeadm"
	"github.com/klotho-k8s/klotho/provider/controlplane/rke2"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
	awsprovider "github.com/klotho-k8s/klotho/provider/infrastructure/aws"
	azureprovider "github.com/klotho-k8s/klotho/provider/infrastructure/azure"
	doprovider "github.com/klotho-k8s/klotho/provider/infrastructure/digitalocean"
	dockerprovider "github.com/klotho-k8s/klotho/provider/infrastructure/docker"
	gcpprovider "github.com/klotho-k8s/klotho/provider/infrastructure/gcp"
	hetznerprovider "github.com/klotho-k8s/klotho/provider/infrastructure/hetzner"
	hypervprovider "github.com/klotho-k8s/klotho/provider/infrastructure/hyperv"
	libvirtprovider "github.com/klotho-k8s/klotho/provider/infrastructure/libvirt"
	linodeprovider "github.com/klotho-k8s/klotho/provider/infrastructure/linode"
	ociprovider "github.com/klotho-k8s/klotho/provider/infrastructure/oci"
	openstackprovider "github.com/klotho-k8s/klotho/provider/infrastructure/openstack"
	proxmoxprovider "github.com/klotho-k8s/klotho/provider/infrastructure/proxmox"
	vsphereprovider "github.com/klotho-k8s/klotho/provider/infrastructure/vsphere"
	vzprovider "github.com/klotho-k8s/klotho/provider/infrastructure/vz"
	"github.com/klotho-k8s/klotho/provider/payload"
	s3payload "github.com/klotho-k8s/klotho/provider/payload/s3"
)

// Provider names the CLI accepts.
const (
	providerProxmox   = "proxmox"
	providerAWS       = "aws"
	providerLinode    = "linode"
	providerGCP       = "gcp"
	providerHetzner   = "hetzner"
	providerDO        = "digitalocean"
	providerVSphere   = "vsphere"
	providerOpenStack = "openstack"
	providerOCI       = "oci"
	providerAzure     = "azure"
	providerDocker    = "docker"
	providerHyperv    = "hyperv"
	providerLibvirt   = "libvirt"
	providerVZ        = "vz"
	cpKubeadm         = "kubeadm"
	cpK0s             = "k0s"
	cpRKE2            = "rke2"
	cpK3s             = "k3s"
)

// app is what every command builds first: configuration, codec, store,
// payload host and logger. Providers are added on demand because they need
// credentials that read-only commands do not.
type app struct {
	cfg   *config.Config
	codec state.RecordCodec
	store backend.Store
	// payload is nil unless the configuration has a payload section.
	payload payload.Host
	log     *slog.Logger
	pve     *gp.Client
}

func stderr(c *cli.Command) io.Writer {
	if w := c.Root().ErrWriter; w != nil {
		return w
	}
	return os.Stderr
}

func stdout(c *cli.Command) io.Writer {
	if w := c.Root().Writer; w != nil {
		return w
	}
	return os.Stdout
}

func stdin(c *cli.Command) io.Reader {
	if r := c.Root().Reader; r != nil {
		return r
	}
	return os.Stdin
}

func newLogger(c *cli.Command) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.Root().String("log-level"))); err != nil {
		return nil, fmt.Errorf("--log-level: %w", err)
	}
	h := slog.NewTextHandler(stderr(c), &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	return slog.New(h), nil
}

// loadConfig reads the configuration selected by the root flags.
func loadConfig(c *cli.Command) (*config.Config, error) {
	cfg, err := config.Load(c.Root().String("config"), c.Root().String("profile"))
	if err != nil {
		return nil, err
	}
	if b := c.Root().String("backend"); b != "" {
		cfg.Backend = b
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// newApp loads configuration and opens the codec and the store.
func newApp(c *cli.Command) (*app, error) {
	log, err := newLogger(c)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(c)
	if err != nil {
		return nil, err
	}
	codec, err := cfg.Codec()
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, codec: codec, log: log}
	switch b := cfg.EffectiveBackend(); b {
	case config.BackendFile:
		s, err := filebackend.New(cfg.RecordsDir(), codec)
		if err != nil {
			return nil, err
		}
		a.store = s
	case config.BackendProxmox:
		client, err := a.proxmoxClient()
		if err != nil {
			return nil, fmt.Errorf("proxmox backend: %w", err)
		}
		a.store = proxmoxbackend.New(client, codec)
	case config.BackendS3:
		s := cfg.S3Backend
		store, err := s3backend.New(context.Background(), s3backend.Config{
			Endpoint:        s.Endpoint,
			Region:          s.Region,
			Bucket:          s.Bucket,
			Prefix:          s.Prefix,
			UsePathStyle:    s.UsePathStyle,
			AccessKeyID:     s.AccessKeyID,
			SecretAccessKey: s.SecretAccessKey,
		}, codec)
		if err != nil {
			return nil, err
		}
		a.store = store
	case config.BackendAzureBlob:
		ab := cfg.AzureBlob
		store, err := azureblobbackend.New(context.Background(), azureblobbackend.Config{
			AccountName:      ab.AccountName,
			Endpoint:         ab.Endpoint,
			Container:        ab.Container,
			Prefix:           ab.Prefix,
			ConnectionString: ab.ConnectionString,
			AccountKey:       ab.AccountKey,
		}, codec)
		if err != nil {
			return nil, err
		}
		a.store = store
	default:
		return nil, fmt.Errorf("backend %q: %w", b, ErrNotImplemented)
	}
	if a.payload, err = a.payloadHost(); err != nil {
		return nil, err
	}
	return a, nil
}

// payloadHost builds the configured payload host, or returns nil when the
// configuration has none. Building one touches no network.
func (a *app) payloadHost() (payload.Host, error) {
	switch k := a.cfg.Payload.Kind; k {
	case "":
		return nil, nil
	case config.PayloadS3:
		s := a.cfg.Payload.S3
		h, err := s3payload.New(context.Background(), s3payload.Config{
			Endpoint:        s.Endpoint,
			Region:          s.Region,
			Bucket:          s.Bucket,
			Prefix:          s.Prefix,
			UsePathStyle:    s.UsePathStyle,
			PresignTTL:      s.TTL(),
			AccessKeyID:     s.AccessKeyID,
			SecretAccessKey: s.SecretAccessKey,
		})
		if err != nil {
			return nil, fmt.Errorf("payload host: %w", err)
		}
		return h, nil
	default:
		return nil, fmt.Errorf("payload host %q: %w (available: %s)", k, ErrNotImplemented, config.PayloadS3)
	}
}

// proxmoxClient connects to the configured Proxmox VE API, once.
func (a *app) proxmoxClient() (*gp.Client, error) {
	if a.pve != nil {
		return a.pve, nil
	}
	if err := a.cfg.CheckProxmox(); err != nil {
		return nil, err
	}
	p := a.cfg.Proxmox
	conn := proxmoxprovider.Config{URL: p.URL, TokenID: p.TokenID, Secret: p.Secret, Insecure: p.Insecure}
	if p.CACertFile != "" {
		pem, err := os.ReadFile(p.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("proxmox.caCertFile: %w", err)
		}
		conn.CACert = pem
	}
	client, err := proxmoxprovider.NewClient(conn)
	if err != nil {
		return nil, err
	}
	a.pve = client
	return client, nil
}

// deps returns library dependencies without providers, enough for List,
// Load and deleting a reservation. Payload is nil unless one is configured.
func (a *app) deps() klotho.Deps {
	return klotho.Deps{Store: a.store, Codec: a.codec, Payload: a.payload, Logger: a.log}
}

// withProviders adds the infrastructure and control plane providers.
// kubeVersion is the fallback Kubernetes version for a control plane object
// that does not set one; it is empty for operations (delete) that do not need
// it.
func (a *app) withProviders(provider, cp, kubeVersion string) (klotho.Deps, error) {
	d := a.deps()
	infra, err := a.infra(provider)
	if err != nil {
		return d, err
	}
	// CAPD runs the control plane inside containers, where kube-proxy cannot
	// write the conntrack sysctls; the provider disables those writes.
	containerNodes := provider == providerDocker
	switch cp {
	case cpKubeadm:
		d.ControlPlane = kubeadmProvider{kubeadm.New(kubeadm.Options{
			KubernetesVersion: kubeVersion,
			CertManagerURL:    a.cfg.CAPI.CertManagerURL,
			OperatorURL:       a.cfg.CAPI.OperatorURL,
			CNIManifestURL:    a.cfg.CAPI.CNIManifestURL,
			ContainerNodes:    containerNodes,
		})}
	case cpK0s:
		d.ControlPlane = k0sProvider{k0s.New(k0s.Options{
			KubernetesVersion: kubeVersion,
			CertManagerURL:    a.cfg.CAPI.CertManagerURL,
			OperatorURL:       a.cfg.CAPI.OperatorURL,
			ContainerNodes:    containerNodes,
		})}
	case cpRKE2:
		d.ControlPlane = rke2Provider{rke2.New(rke2.Options{
			KubernetesVersion: kubeVersion,
			CertManagerURL:    a.cfg.CAPI.CertManagerURL,
			OperatorURL:       a.cfg.CAPI.OperatorURL,
			CNIManifestURL:    a.cfg.CAPI.CNIManifestURL,
			ContainerNodes:    containerNodes,
		})}
	case cpK3s:
		d.ControlPlane = k3sProvider{k3s.New(k3s.Options{
			KubernetesVersion: kubeVersion,
			CertManagerURL:    a.cfg.CAPI.CertManagerURL,
			OperatorURL:       a.cfg.CAPI.OperatorURL,
			CNIManifestURL:    a.cfg.CAPI.CNIManifestURL,
			ContainerNodes:    containerNodes,
		})}
	default:
		return d, fmt.Errorf("control plane provider %q: %w (available: %s, %s, %s, %s)", cp, ErrNotImplemented, cpKubeadm, cpK0s, cpRKE2, cpK3s)
	}
	d.Infra = infra
	return d, nil
}

func (a *app) infra(provider string) (infrastructure.Provider, error) {
	switch provider {
	case providerProxmox:
		client, err := a.proxmoxClient()
		if err != nil {
			return nil, err
		}
		p := a.cfg.Proxmox
		pc := proxmoxprovider.Config{
			URL:                 p.URL,
			TokenID:             p.TokenID,
			Secret:              p.Secret,
			Insecure:            p.Insecure,
			Node:                p.Node,
			TemplateID:          p.TemplateID,
			TemplateTag:         p.TemplateTag,
			TemplateTags:        p.Template.Tags,
			TemplateMatchPolicy: p.Template.MatchPolicy,
			ISOStorage:          p.ISOStorage,
			Bridge:              p.Bridge,
			Cores:               p.Cores,
			Sockets:             p.Sockets,
			MemoryMiB:           p.MemoryMiB,
			Pool:                p.Pool,
		}
		if p.CACertFile != "" {
			pem, err := os.ReadFile(p.CACertFile)
			if err != nil {
				return nil, fmt.Errorf("proxmox.caCertFile: %w", err)
			}
			pc.CACert = pem
		}
		s := p.Sidecar
		pc.Sidecar = proxmoxprovider.SidecarConfig{
			TemplateStorage:    s.TemplateStorage,
			RootfsStorage:      s.RootfsStorage,
			Bridge:             s.Bridge,
			IP:                 s.IP,
			Gateway:            s.Gateway,
			DNS:                s.DNS,
			CurlURL:            s.CurlURL,
			CurlSHA256:         s.CurlSHA256,
			JqURL:              s.JqURL,
			JqSHA256:           s.JqSHA256,
			AlpineRootfsURL:    s.AlpineRootfsURL,
			AlpineRootfsSHA256: s.AlpineRootfsSHA256,
			MemoryMiB:          s.MemoryMiB,
		}
		if s.TokenTTL != "" {
			ttl, err := time.ParseDuration(s.TokenTTL)
			if err != nil {
				return nil, fmt.Errorf("proxmox.sidecar.tokenTtl: %w", err)
			}
			pc.Sidecar.TokenTTL = ttl
		}
		if len(p.VMIDRange) == 2 {
			pc.VMIDRange = [2]int{p.VMIDRange[0], p.VMIDRange[1]}
		}
		return proxmoxInfra{proxmoxprovider.New(client, pc)}, nil
	case providerAWS:
		a := a.cfg.AWS
		ac := awsprovider.Config{
			Region:               a.Region,
			Profile:              a.Profile,
			VPCID:                a.VPCID,
			SubnetID:             a.SubnetID,
			SecurityGroupIDs:     a.SecurityGroupIDs,
			AMIID:                a.AMIID,
			InstanceType:         a.InstanceType,
			SSHKeyName:           a.SSHKeyName,
			IAMInstanceProfile:   a.IAMInstanceProfile,
			ControlPlaneEndpoint: a.ControlPlaneEndpoint,
			AllocateElasticIP:    a.AllocateElasticIP,
			Port:                 a.Port,
			Spot:                 a.Spot,
			PublicIP:             a.PublicIP,
		}
		client, err := awsprovider.NewClient(context.Background(), ac)
		if err != nil {
			return nil, err
		}
		return awsInfra{awsprovider.New(client, ac)}, nil
	case providerLinode:
		l := a.cfg.Linode
		lc := linodeprovider.Config{
			Token:                l.Token,
			Region:               l.Region,
			Type:                 l.Type,
			Image:                l.Image,
			AuthorizedKeys:       l.AuthorizedKeys,
			RootPass:             l.RootPass,
			Tags:                 l.Tags,
			FirewallID:           l.FirewallID,
			ControlPlaneEndpoint: l.ControlPlaneEndpoint,
			Port:                 l.Port,
		}
		client, err := linodeprovider.NewClient(lc)
		if err != nil {
			return nil, err
		}
		return linodeInfra{linodeprovider.New(client, lc)}, nil
	case providerGCP:
		g := a.cfg.GCP
		if g.Project == "" {
			return nil, fmt.Errorf("gcp provider: project is required (set gcp.project)")
		}
		credsJSON, err := a.gcpCredentials()
		if err != nil {
			return nil, err
		}
		publicIP := true
		if g.PublicIP != nil {
			publicIP = *g.PublicIP
		}
		gc := gcpprovider.Config{
			Project:              g.Project,
			Region:               g.Region,
			Zone:                 g.Zone,
			MachineType:          g.MachineType,
			Image:                g.Image,
			ImageProject:         g.ImageProject,
			ImageFamily:          g.ImageFamily,
			DiskSizeGB:           g.DiskSizeGB,
			DiskType:             g.DiskType,
			Network:              g.Network,
			Subnet:               g.Subnet,
			ServiceAccount:       g.ServiceAccount,
			NetworkTags:          g.NetworkTags,
			AuthorizedKeys:       g.AuthorizedKeys,
			LoginUser:            g.LoginUser,
			PublicIP:             publicIP,
			Preemptible:          g.Preemptible,
			ControlPlaneEndpoint: g.ControlPlaneEndpoint,
			Port:                 g.Port,
			CredentialsJSON:      credsJSON,
		}
		client, err := gcpprovider.NewClient(context.Background(), credsJSON)
		if err != nil {
			return nil, err
		}
		return gcpInfra{gcpprovider.New(client, gc)}, nil
	case providerHetzner:
		h := a.cfg.Hetzner
		hc := hetznerprovider.Config{
			Token:                h.Token,
			Location:             h.Location,
			ServerType:           h.ServerType,
			Image:                h.Image,
			SSHKeys:              h.SSHKeys,
			Labels:               h.Labels,
			ControlPlaneEndpoint: h.ControlPlaneEndpoint,
			Port:                 h.Port,
		}
		client, err := hetznerprovider.NewClient(hc)
		if err != nil {
			return nil, err
		}
		return hetznerInfra{hetznerprovider.New(client, hc)}, nil
	case providerDO:
		d := a.cfg.DigitalOcean
		dc := doprovider.Config{
			Token:                d.Token,
			Region:               d.Region,
			Size:                 d.Size,
			Image:                d.Image,
			SSHKeys:              d.SSHKeys,
			Tags:                 d.Tags,
			VPCUUID:              d.VPCUUID,
			LoadBalancerID:       d.LoadBalancerID,
			ControlPlaneEndpoint: d.ControlPlaneEndpoint,
			Port:                 d.Port,
		}
		client, err := doprovider.NewClient(dc)
		if err != nil {
			return nil, err
		}
		return doInfra{doprovider.New(client, dc)}, nil
	case providerOpenStack:
		o := a.cfg.OpenStack
		if o.Cloud == "" {
			return nil, fmt.Errorf("openstack provider: cloud is required (set openstack.cloud or OS_CLOUD)")
		}
		cloudsYAML, err := a.openStackCloudsYAML()
		if err != nil {
			return nil, err
		}
		oc := openstackprovider.Config{
			Cloud:                o.Cloud,
			CloudsYAML:           o.CloudsYAML,
			CloudsYAMLData:       cloudsYAML,
			Region:               o.Region,
			Network:              o.Network,
			Subnet:               o.Subnet,
			ExternalNetwork:      o.ExternalNetwork,
			Flavor:               o.Flavor,
			Image:                o.Image,
			SSHKeyName:           o.SSHKeyName,
			SecurityGroups:       o.SecurityGroups,
			AvailabilityZone:     o.AvailabilityZone,
			ControlPlaneEndpoint: o.ControlPlaneEndpoint,
			Port:                 o.Port,
		}
		client, err := openstackprovider.NewClient(context.Background(), o.Cloud, o.CloudsYAML, o.Region)
		if err != nil {
			return nil, err
		}
		return openstackInfra{openstackprovider.New(client, oc)}, nil
	case providerAzure:
		az := a.cfg.Azure
		ac := azureprovider.Config{
			SubscriptionID: az.SubscriptionID,
			TenantID:       az.TenantID,
			ClientID:       az.ClientID,
			ClientSecret:   az.ClientSecret,
			Location:       az.Location,
			ResourceGroup:  az.ResourceGroup,
			VNet:           az.VNet,
			Subnet:         az.Subnet,
			VMSize:         az.VMSize,
			Image: azureprovider.ImageRef{
				Publisher: az.ImagePublisher,
				Offer:     az.ImageOffer,
				SKU:       az.ImageSKU,
				Version:   az.ImageVersion,
			},
			AdminUsername:        az.AdminUsername,
			SSHPublicKey:         az.SSHPublicKey,
			Spot:                 az.Spot,
			ControlPlaneEndpoint: az.ControlPlaneEndpoint,
			Port:                 az.Port,
		}
		client, err := azureprovider.NewClient(ac)
		if err != nil {
			return nil, err
		}
		return azureInfra{azureprovider.New(client, ac)}, nil
	case providerOCI:
		o := a.cfg.OCI
		assignPublicIP := true
		if o.AssignPublicIP != nil {
			assignPublicIP = *o.AssignPublicIP
		}
		oc := ociprovider.Config{
			CompartmentID:        o.CompartmentID,
			Region:               o.Region,
			AvailabilityDomain:   o.AvailabilityDomain,
			VCNID:                o.VCNID,
			SubnetID:             o.SubnetID,
			NSGIDs:               o.NSGIDs,
			ImageID:              o.ImageID,
			Shape:                o.Shape,
			OCPUs:                o.OCPUs,
			MemoryGBs:            o.MemoryGBs,
			BootVolumeGBs:        o.BootVolumeGBs,
			AssignPublicIP:       assignPublicIP,
			SSHAuthorizedKeys:    o.SSHAuthorizedKeys,
			FreeformTags:         o.FreeformTags,
			ResourceIdentifier:   o.ResourceIdentifier,
			ControlPlaneEndpoint: o.ControlPlaneEndpoint,
			Port:                 o.Port,
			ConfigFile:           o.ConfigFile,
			Profile:              o.Profile,
			UseInstancePrincipal: o.UseInstancePrincipal,
		}
		client, err := ociprovider.NewClient(oc)
		if err != nil {
			return nil, err
		}
		return ociInfra{ociprovider.New(client, oc)}, nil
	case providerDocker:
		d := a.cfg.Docker
		return dockerInfra{dockerprovider.New(nil, dockerprovider.Config{
			Runtime:      d.Runtime,
			NodeImage:    d.NodeImage,
			Network:      d.Network,
			LBImage:      d.LBImage,
			Socket:       d.Socket,
			CleanupImage: d.CleanupImage,
		})}, nil
	case providerHyperv:
		h := a.cfg.Hyperv
		return hypervInfra{hypervprovider.New(nil, hypervprovider.Config{
			Switch:       h.Switch,
			Image:        h.Image,
			VMPath:       h.VMPath,
			ISODir:       h.ISODir,
			MemoryMiB:    h.MemoryMiB,
			CPUs:         h.CPUs,
			EndpointHost: h.EndpointHost,
			SecureBoot:   h.SecureBoot,
		})}, nil
	case providerLibvirt:
		l := a.cfg.Libvirt
		return libvirtInfra{libvirtprovider.New(nil, libvirtprovider.Config{
			URI:           l.URI,
			ControllerURI: l.ControllerURI,
			StoragePool:   l.StoragePool,
			Image:         l.Image,
			Network:       l.Network,
			NetworkType:   l.NetworkType,
			MemoryMiB:     l.MemoryMiB,
			CPUs:          l.CPUs,
			DiskGiB:       l.DiskGiB,
			EndpointHost:  l.EndpointHost,
		})}, nil
	case providerVZ:
		v := a.cfg.VZ
		return vzInfra{vzprovider.New(nil, vzprovider.Config{
			Endpoint:     v.Endpoint,
			CA:           v.CA,
			ServerName:   v.ServerName,
			Token:        v.Token,
			Host:         v.Host,
			Image:        v.Image,
			MemoryMiB:    v.MemoryMiB,
			CPUs:         v.CPUs,
			EndpointHost: v.EndpointHost,
		})}, nil
	case providerVSphere:
		v := a.cfg.VSphere
		return vsphereInfra{vsphereprovider.New(nil, vsphereprovider.Config{
			Server:           v.Server,
			Username:         v.Username,
			Password:         v.Password,
			Thumbprint:       v.Thumbprint,
			Insecure:         v.Insecure,
			Datacenter:       v.Datacenter,
			Datastore:        v.Datastore,
			Folder:           v.Folder,
			ResourcePool:     v.ResourcePool,
			Network:          v.Network,
			Template:         v.Template,
			CloneMode:        v.CloneMode,
			NumCPUs:          v.NumCPUs,
			MemoryMiB:        v.MemoryMiB,
			DiskGiB:          v.DiskGiB,
			EndpointIP:       v.EndpointIP,
			Port:             v.Port,
			VIPInterface:     v.VIPInterface,
			SSHAuthorizedKey: v.SSHAuthorizedKey,
		})}, nil
	default:
		return nil, fmt.Errorf("infrastructure provider %q: %w (available: %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)", provider, ErrNotImplemented, providerProxmox, providerDocker, providerAWS, providerLinode, providerGCP, providerHetzner, providerDO, providerAzure, providerOCI, providerHyperv, providerLibvirt, providerVZ, providerVSphere, providerOpenStack)
	}
}

// gcpCredentials reads the configured service-account key JSON, or returns nil
// (Application Default Credentials) when no file is configured.
func (a *app) gcpCredentials() ([]byte, error) {
	if a.cfg.GCP.CredentialsFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(a.cfg.GCP.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("gcp.credentialsFile: %w", err)
	}
	return b, nil
}

// openStackCloudsYAML reads the clouds.yaml whose contents are handed, base64-
// encoded, to the in-cluster CAPO controller as a per-cluster credential Secret.
// It reads the configured path, else the standard OpenStack locations. It is not
// an error to find none here (the host-side client may still authenticate from
// the environment); the generated Secret is then empty and the live run fails at
// the controller, which is clearer than failing create for a host that has the
// credential in its environment.
func (a *app) openStackCloudsYAML() ([]byte, error) {
	candidates := []string{a.cfg.OpenStack.CloudsYAML, os.Getenv("OS_CLIENT_CONFIG_FILE"), "clouds.yaml"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "openstack", "clouds.yaml"))
	}
	candidates = append(candidates, filepath.Join("/etc", "openstack", "clouds.yaml"))
	for _, path := range candidates {
		if path == "" {
			continue
		}
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return b, nil
		}
	}
	return nil, nil
}

// providerSecretData is what the infrastructure provider's controller needs
// to reach the infrastructure, handed to the operator as its configSecret.
func (a *app) providerSecretData(provider string) map[string]string {
	switch provider {
	case providerProxmox:
		p := a.cfg.Proxmox
		url := strings.TrimSuffix(strings.TrimSuffix(p.URL, "/"), "/api2/json")
		data := map[string]string{
			"PROXMOX_URL":    url,
			"PROXMOX_TOKEN":  p.TokenID,
			"PROXMOX_SECRET": p.Secret,
		}
		if len(p.AdditionalURLs) > 0 {
			data["PROXMOX_ADDITIONAL_URLS"] = strings.Join(p.AdditionalURLs, ",")
		}
		if p.EndpointDiscovery {
			data["PROXMOX_ENDPOINT_DISCOVERY"] = "true"
		}
		return data
	case providerAWS:
		// CAPA's infrastructure-components.yaml substitutes
		// ${AWS_B64ENCODED_CREDENTIALS}; the operator fails to install the
		// provider if the variable is unset. klotho supplies it empty so CAPA
		// falls back to the default AWS credential chain — the control-plane
		// node's instance profile — rather than a baked-in credential.
		return map[string]string{"AWS_B64ENCODED_CREDENTIALS": ""}
	case providerLinode:
		if t := a.cfg.Linode.Token; t != "" {
			return map[string]string{"LINODE_TOKEN": t}
		}
	case providerHetzner:
		if t := a.cfg.Hetzner.Token; t != "" {
			return map[string]string{"HCLOUD_TOKEN": t}
		}
	case providerDO:
		// CAPDO's infrastructure-components.yaml substitutes
		// ${DO_B64ENCODED_CREDENTIALS} (base64 of the API token) into the Secret
		// the controller reads as DIGITALOCEAN_ACCESS_TOKEN. Hand it over the same
		// way CAPG hands over GCP_B64ENCODED_CREDENTIALS.
		if t := a.cfg.DigitalOcean.Token; t != "" {
			return map[string]string{"DO_B64ENCODED_CREDENTIALS": base64.StdEncoding.EncodeToString([]byte(t))}
		}
	case providerGCP:
		if f := a.cfg.GCP.CredentialsFile; f != "" {
			if b, err := os.ReadFile(f); err == nil && len(b) > 0 {
				return map[string]string{"GCP_B64ENCODED_CREDENTIALS": base64.StdEncoding.EncodeToString(b)}
			}
		}
	case providerOCI:
		// CAPOCI's in-cluster controller authenticates with an OCIClusterIdentity
		// (a referenced secret) or, on a self-managed node in a dynamic group, the
		// instance principal — not a substituted env credential. klotho stores no
		// raw OCI key, so no configSecret is handed over here; the identity is set
		// up out of band (see provider/infrastructure/oci/TESTING.md).
		return nil
	case providerAzure:
		az := a.cfg.Azure
		data := map[string]string{}
		if az.SubscriptionID != "" {
			data["AZURE_SUBSCRIPTION_ID"] = az.SubscriptionID
		}
		if az.TenantID != "" {
			data["AZURE_TENANT_ID"] = az.TenantID
		}
		if az.ClientID != "" {
			data["AZURE_CLIENT_ID"] = az.ClientID
		}
		if az.ClientSecret != "" {
			data["AZURE_CLIENT_SECRET"] = az.ClientSecret
		}
		if len(data) > 0 {
			return data
		}
	case providerHyperv:
		h := a.cfg.Hyperv
		data := map[string]string{}
		if ep := h.ResolvedAgentEndpoint(); ep != "" {
			data["HYPERV_AGENT_ENDPOINT"] = ep
		}
		if ca, err := h.ResolvedAgentCA(); err == nil && ca != "" {
			// The operator substitutes configSecret values into the provider
			// components with a plain ${VAR} replacement, which mangles a
			// multi-line PEM. Hand the CA over base64-encoded; the controller
			// decodes it in memory (HYPERV_AGENT_CA).
			data["HYPERV_AGENT_CA"] = base64.StdEncoding.EncodeToString([]byte(ca))
		}
		if tok, err := h.ResolvedAgentToken(); err == nil && tok != "" {
			data["HYPERV_AGENT_TOKEN"] = tok
		}
		if len(data) == 0 {
			return nil
		}
		return data
	case providerLibvirt:
		// The in-cluster Cluster API libvirt controller runs in a pod, so it
		// needs a network URI (qemu+tcp:// or qemu+tls://), not the local socket
		// klotho the CLI may dial: ControllerURI resolves that. TLS/SSH
		// credentials for a remote URI are delivered separately as a secretRef
		// on the LibvirtCluster, not here.
		return map[string]string{"LIBVIRT_URI": libvirtprovider.ControllerURI(libvirtprovider.Config{
			URI:           a.cfg.Libvirt.URI,
			ControllerURI: a.cfg.Libvirt.ControllerURI,
		})}
	case providerVZ:
		// The in-cluster Cluster API vz controller reaches the same capvz-daemon
		// klotho dialed; hand it the daemon's gRPC endpoint and bearer token.
		// The CA is delivered separately (the controller's --vz-daemon-ca, or the
		// system roots), not as a substituted variable.
		v := a.cfg.VZ
		data := map[string]string{}
		if v.Endpoint != "" {
			data["VZ_DAEMON_ENDPOINT"] = v.Endpoint
		}
		if v.Token != "" {
			data["VZ_DAEMON_TOKEN"] = v.Token
		}
		if len(data) > 0 {
			return data
		}
	case providerVSphere:
		// The flat template authenticates the in-cluster CAPV controller through a
		// per-cluster identity Secret (built from VSPHERE_USERNAME/VSPHERE_PASSWORD
		// in the bundle). These configSecret values cover a CAPV build that also
		// reads global manager credentials; an empty username leaves them unset.
		v := a.cfg.VSphere
		data := map[string]string{}
		if v.Username != "" {
			data["VSPHERE_USERNAME"] = v.Username
		}
		if v.Password != "" {
			data["VSPHERE_PASSWORD"] = v.Password
		}
		if len(data) > 0 {
			return data
		}
	}
	return nil
}

// proxmoxInfra adds the provider's tag convention to the Proxmox provider.
type proxmoxInfra struct{ *proxmoxprovider.Provider }

func (proxmoxInfra) Tag(cluster string) string { return proxmoxprovider.Tag(cluster) }

// awsInfra adds the provider's tag convention to the AWS provider.
type awsInfra struct{ *awsprovider.Provider }

func (awsInfra) Tag(cluster string) string { return awsprovider.Tag(cluster) }

// linodeInfra adds the provider's tag convention to the Linode provider.
type linodeInfra struct{ *linodeprovider.Provider }

func (linodeInfra) Tag(cluster string) string { return linodeprovider.Tag(cluster) }

// gcpInfra adds the provider's label convention to the GCP provider.
type gcpInfra struct{ *gcpprovider.Provider }

func (gcpInfra) Tag(cluster string) string { return gcpprovider.Tag(cluster) }

// hetznerInfra adds the provider's label convention to the Hetzner provider.
type hetznerInfra struct{ *hetznerprovider.Provider }

func (hetznerInfra) Tag(cluster string) string { return hetznerprovider.Tag(cluster) }

// doInfra adds the provider's tag convention to the DigitalOcean provider.
type doInfra struct{ *doprovider.Provider }

func (doInfra) Tag(cluster string) string { return doprovider.Tag(cluster) }

// openstackInfra adds the provider's tag convention to the OpenStack provider.
type openstackInfra struct{ *openstackprovider.Provider }

func (openstackInfra) Tag(cluster string) string { return openstackprovider.Tag(cluster) }

// azureInfra adds the provider's tag convention to the Azure provider.
type azureInfra struct{ *azureprovider.Provider }

func (azureInfra) Tag(cluster string) string { return azureprovider.Tag(cluster) }

// ociInfra adds the provider's freeform-tag convention to the OCI provider.
type ociInfra struct{ *ociprovider.Provider }

func (ociInfra) Tag(cluster string) string { return ociprovider.Tag(cluster) }

// dockerInfra adds the provider's cluster-label convention to the Docker
// provider.
type dockerInfra struct{ *dockerprovider.Provider }

func (dockerInfra) Tag(cluster string) string { return dockerprovider.Tag(cluster) }

// hypervInfra adds the provider's tag convention to the Hyper-V provider.
type hypervInfra struct{ *hypervprovider.Provider }

func (hypervInfra) Tag(cluster string) string { return hypervprovider.Tag(cluster) }

// libvirtInfra adds the provider's tag convention to the libvirt provider.
type libvirtInfra struct{ *libvirtprovider.Provider }

func (libvirtInfra) Tag(cluster string) string { return libvirtprovider.Tag(cluster) }

// vzInfra adds the provider's tag convention to the vz provider.
type vzInfra struct{ *vzprovider.Provider }

func (vzInfra) Tag(cluster string) string { return vzprovider.Tag(cluster) }

// vsphereInfra adds the provider's tag convention to the vSphere provider.
type vsphereInfra struct{ *vsphereprovider.Provider }

func (vsphereInfra) Tag(cluster string) string { return vsphereprovider.Tag(cluster) }

// kubeadmProvider adds the kubeadm provider's bootstrap machine naming.
type kubeadmProvider struct{ *kubeadm.Provider }

func (kubeadmProvider) BootstrapMachineName(cluster string) string {
	return kubeadm.BootstrapMachineName(cluster)
}

// k0sProvider adds the k0s provider's bootstrap machine naming.
type k0sProvider struct{ *k0s.Provider }

func (k0sProvider) BootstrapMachineName(cluster string) string {
	return k0s.BootstrapMachineName(cluster)
}

// rke2Provider adds the rke2 provider's bootstrap machine naming.
type rke2Provider struct{ *rke2.Provider }

func (rke2Provider) BootstrapMachineName(cluster string) string {
	return rke2.BootstrapMachineName(cluster)
}

// k3sProvider adds the k3s provider's bootstrap machine naming.
type k3sProvider struct{ *k3s.Provider }

func (k3sProvider) BootstrapMachineName(cluster string) string {
	return k3s.BootstrapMachineName(cluster)
}

var (
	_ klotho.Tagger = proxmoxInfra{}
	_ klotho.Tagger = awsInfra{}
	_ klotho.Tagger = linodeInfra{}
	_ klotho.Tagger = gcpInfra{}
	_ klotho.Tagger = hetznerInfra{}
	_ klotho.Tagger = doInfra{}
	_ klotho.Tagger = openstackInfra{}
	_ klotho.Tagger = azureInfra{}
	_ klotho.Tagger = ociInfra{}
	_ klotho.Tagger = hypervInfra{}
	_ klotho.Tagger = libvirtInfra{}
	_ klotho.Tagger = vzInfra{}
	_ klotho.Tagger = vsphereInfra{}
	// libvirt's MachineTemplate has no tags field (the CRD rejects the default
	// spec.template.spec.tags); it tags via the domain description instead.
	_ klotho.MachineTemplateTagger = libvirtInfra{}
	// vSphere's VSphereMachineTemplate likewise rejects a tags string list (its
	// field is tagIDs); it tags via the VM annotation instead.
	_ klotho.MachineTemplateTagger = vsphereInfra{}
	// DigitalOcean's DOMachineTemplate uses spec.template.spec.additionalTags (a
	// string list), not the default spec.template.spec.tags CAPDO rejects.
	_ klotho.MachineTemplateTagger = doInfra{}
	_ klotho.BootstrapNamer        = kubeadmProvider{}
	_ klotho.BootstrapNamer        = k0sProvider{}
	_ klotho.BootstrapNamer        = rke2Provider{}
	_ klotho.BootstrapNamer        = k3sProvider{}
	_ controlplane.Provider        = kubeadmProvider{}
	_ controlplane.Provider        = k0sProvider{}
	_ controlplane.Provider        = rke2Provider{}
	_ controlplane.Provider        = k3sProvider{}
	_ infrastructure.Provider      = proxmoxInfra{}
	_ infrastructure.Provider      = awsInfra{}
	_ infrastructure.Provider      = linodeInfra{}
	_ infrastructure.Provider      = gcpInfra{}
	_ infrastructure.Provider      = hetznerInfra{}
	_ infrastructure.Provider      = doInfra{}
	_ infrastructure.Provider      = openstackInfra{}
	_ infrastructure.Provider      = azureInfra{}
	_ infrastructure.Provider      = ociInfra{}
	_ infrastructure.Provider      = hypervInfra{}
	_ infrastructure.Provider      = libvirtInfra{}
	_ infrastructure.Provider      = vzInfra{}
	_ infrastructure.Provider      = vsphereInfra{}
)

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// credentialArgs are the root flags a kubeconfig's exec block must pass so
// "klotho credential" finds the same configuration.
func (a *app) credentialArgs(c *cli.Command) []string {
	var args []string
	if p := c.Root().String("config"); p != "" {
		args = append(args, "--config", a.cfg.Path)
	}
	if p := a.cfg.Profile; p != "" {
		args = append(args, "--profile", p)
	}
	if b := c.Root().String("backend"); b != "" {
		args = append(args, "--backend", b)
	}
	return args
}
