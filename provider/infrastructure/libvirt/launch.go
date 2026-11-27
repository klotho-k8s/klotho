package libvirt

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// addressTimeout bounds the wait for the guest to report an IPv4 address.
const addressTimeout = 5 * time.Minute

// addressPoll is how often the guest address is polled.
const addressPoll = 5 * time.Second

// Launch boots the bootstrap node as a single libvirt domain: it generates the
// domain UUID, defines the domain from a qcow2 overlay of the base image with a
// NoCloud cidata seed carrying userData (seeded with the UUID as the meta-data
// instance-id), starts it, and waits for an IPv4 address. The Cluster API
// libvirt provider later adopts this domain because its name matches the
// LibvirtMachine's spec.domainName and it is already running.
//
// The returned node's ID is "libvirt://<host>/<uuid>" and its Name is the
// domain name. On any failure after the domain is defined, the domain and its
// volumes are removed on a best-effort basis and the original error is returned.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	if len(userData) == 0 {
		return nil, errorf("empty user-data")
	}
	r := p.resolve(spec.Variables)
	if r.Image == "" {
		return nil, errorf("no base image: set libvirt.image or the %s variable", VarImage)
	}
	name := DomainName(spec.Name)

	if _, err := p.client.GetDomain(ctx, name); err == nil {
		return nil, errorf("bootstrap domain %q already exists; delete the cluster first", name)
	} else if !errors.Is(err, ErrDomainNotFound) {
		return nil, err
	}

	uuid, err := newUUID()
	if err != nil {
		return nil, errorf("generating domain UUID: %w", err)
	}
	ciData, err := buildCIData(uuid, name, userData)
	if err != nil {
		return nil, err
	}

	in := CreateDomainInput{
		Name:          name,
		UUID:          uuid,
		Description:   Tag(spec.Name),
		MemoryBytes:   int64(r.MemoryMiB) << 20,
		VCPU:          r.CPUs,
		BaseImage:     r.Image,
		OverlayName:   OverlayName(spec.Name),
		CIDataName:    CIDataName(spec.Name),
		StoragePool:   r.StoragePool,
		DiskSizeBytes: int64(r.DiskGiB) << 30,
		NetworkName:   r.Network,
		NetworkType:   r.NetworkType,
		CIData:        ciData,
	}
	dom, err := p.client.CreateDomain(ctx, in)
	if err != nil {
		return nil, errorf("creating bootstrap domain %q: %w", name, err)
	}
	// CreateDomain assigns the UUID from the XML; trust the returned one.
	providerID := ProviderID(p.client.Host(), dom.UUID)

	node, err := p.start(ctx, name, providerID)
	if err != nil {
		p.cleanup(ctx, name)
		return nil, errorf("launching %q: %w", name, err)
	}
	return node, nil
}

// start boots the defined domain and waits for an address, returning the node.
func (p *Provider) start(ctx context.Context, name, providerID string) (*cluster.Node, error) {
	if err := p.client.StartDomain(ctx, name); err != nil {
		return nil, err
	}
	addrs, err := p.waitForAddress(ctx, name)
	if err != nil {
		return nil, err
	}
	return &cluster.Node{ID: providerID, Name: name, Addresses: addrs}, nil
}

// waitForAddress polls the guest until it reports at least one IPv4 address or
// the timeout elapses.
func (p *Provider) waitForAddress(ctx context.Context, name string) ([]string, error) {
	deadline := time.Now().Add(addressTimeout)
	for {
		addrs, err := p.client.GetDomainIPs(ctx, name)
		if err == nil {
			if v4 := ipv4(addrs); len(v4) > 0 {
				return v4, nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, errorf("waiting for %q to report an address: %w", name, err)
			}
			return nil, errorf("timed out waiting for %q to report an IPv4 address", name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(addressPoll):
		}
	}
}

// ipv4 keeps only the IPv4 addresses.
func ipv4(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			out = append(out, a)
		}
	}
	return out
}

// cleanup removes a half-created domain and its volumes, best effort.
func (p *Provider) cleanup(ctx context.Context, name string) {
	_ = p.client.DestroyDomain(ctx, name)
	_ = p.client.UndefineDomain(ctx, name)
}
