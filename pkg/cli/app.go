package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	gp "github.com/luthermonson/go-proxmox"
	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/config"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
	filebackend "github.com/klotho-k8s/klotho/provider/backend/file"
	proxmoxbackend "github.com/klotho-k8s/klotho/provider/backend/proxmox"
	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/controlplane/kubeadm"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
	awsprovider "github.com/klotho-k8s/klotho/provider/infrastructure/aws"
	dockerprovider "github.com/klotho-k8s/klotho/provider/infrastructure/docker"
	proxmoxprovider "github.com/klotho-k8s/klotho/provider/infrastructure/proxmox"
	"github.com/klotho-k8s/klotho/provider/payload"
	s3payload "github.com/klotho-k8s/klotho/provider/payload/s3"
)

// Provider names the CLI accepts.
const (
	providerProxmox = "proxmox"
	providerAWS     = "aws"
	providerDocker  = "docker"
	cpKubeadm       = "kubeadm"
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
func (a *app) withProviders(provider, cp string, cpOpts kubeadm.Options) (klotho.Deps, error) {
	d := a.deps()
	infra, err := a.infra(provider)
	if err != nil {
		return d, err
	}
	switch cp {
	case cpKubeadm:
		cpOpts.CertManagerURL = firstNonEmpty(cpOpts.CertManagerURL, a.cfg.CAPI.CertManagerURL)
		cpOpts.OperatorURL = firstNonEmpty(cpOpts.OperatorURL, a.cfg.CAPI.OperatorURL)
		cpOpts.CNIManifestURL = firstNonEmpty(cpOpts.CNIManifestURL, a.cfg.CAPI.CNIManifestURL)
		// CAPD runs the control plane inside containers, where kube-proxy
		// cannot write the conntrack sysctls; have kubeadm disable those writes.
		cpOpts.ContainerNodes = provider == providerDocker
		d.ControlPlane = kubeadmProvider{kubeadm.New(cpOpts)}
	default:
		return d, fmt.Errorf("control plane provider %q: %w (available: %s)", cp, ErrNotImplemented, cpKubeadm)
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
	case providerDocker:
		d := a.cfg.Docker
		return dockerInfra{dockerprovider.New(nil, dockerprovider.Config{
			Runtime:   d.Runtime,
			NodeImage: d.NodeImage,
			Network:   d.Network,
			LBImage:   d.LBImage,
			Socket:    d.Socket,
		})}, nil
	default:
		return nil, fmt.Errorf("infrastructure provider %q: %w (available: %s, %s, %s)", provider, ErrNotImplemented, providerProxmox, providerDocker, providerAWS)
	}
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
	}
	return nil
}

// proxmoxInfra adds the provider's tag convention to the Proxmox provider.
type proxmoxInfra struct{ *proxmoxprovider.Provider }

func (proxmoxInfra) Tag(cluster string) string { return proxmoxprovider.Tag(cluster) }

// awsInfra adds the provider's tag convention to the AWS provider.
type awsInfra struct{ *awsprovider.Provider }

func (awsInfra) Tag(cluster string) string { return awsprovider.Tag(cluster) }

// dockerInfra adds the provider's cluster-label convention to the Docker
// provider.
type dockerInfra struct{ *dockerprovider.Provider }

func (dockerInfra) Tag(cluster string) string { return dockerprovider.Tag(cluster) }

// kubeadmProvider adds the kubeadm provider's bootstrap machine naming.
type kubeadmProvider struct{ *kubeadm.Provider }

func (kubeadmProvider) BootstrapMachineName(cluster string) string {
	return kubeadm.BootstrapMachineName(cluster)
}

var (
	_ klotho.Tagger           = proxmoxInfra{}
	_ klotho.Tagger           = awsInfra{}
	_ klotho.BootstrapNamer   = kubeadmProvider{}
	_ controlplane.Provider   = kubeadmProvider{}
	_ infrastructure.Provider = proxmoxInfra{}
	_ infrastructure.Provider = awsInfra{}
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
