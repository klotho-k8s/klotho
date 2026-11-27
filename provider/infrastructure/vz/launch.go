package vz

import (
	"context"
	"net"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// addressTimeout bounds the wait for the guest to report an IPv4 address.
const addressTimeout = 5 * time.Minute

// addressPoll is how often the guest address is polled.
const addressPoll = 5 * time.Second

// Launch boots the bootstrap node as a single vz VM through the capvz-daemon: it
// registers the VM (the daemon copies the base image and assigns a vmid), hands
// the daemon the bootstrap user-data so it builds and records the NoCloud cidata
// seed, starts the VM's runner, and waits for an IPv4 address. The Cluster API vz
// provider later adopts this VM because its name matches the VZMachine's
// spec.vmName and it is already running.
//
// The returned node's ID is "vz://<host>/<vmid>" and its Name is the VM name. On
// any failure after the VM is registered, the VM is removed on a best-effort
// basis and the original error is returned.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	if len(userData) == 0 {
		return nil, errorf("empty user-data")
	}
	r := p.resolve(spec.Variables)
	if r.Image == "" {
		return nil, errorf("no base image: set vz.image or the %s variable", VarImagePath)
	}
	name := VMName(spec.Name)

	if ok, err := p.client.Exists(ctx, name); err != nil {
		return nil, err
	} else if ok {
		return nil, errorf("bootstrap VM %q already exists; delete the cluster first", name)
	}

	vm, err := p.client.CreateVM(ctx, CreateVMInput{
		Name:        name,
		MemoryBytes: int64(r.MemoryMiB) << 20,
		CPUCount:    r.CPUs,
		DiskImage:   r.Image,
		Notes:       Tag(spec.Name),
	})
	if err != nil {
		return nil, errorf("creating bootstrap VM %q: %w", name, err)
	}

	node, err := p.boot(ctx, name, ProviderID(r.Host, vm.ID), userData)
	if err != nil {
		p.cleanup(ctx, name)
		return nil, errorf("launching %q: %w", name, err)
	}
	return node, nil
}

// boot attaches the cloud-init seed, starts the VM and waits for an address,
// returning the node.
func (p *Provider) boot(ctx context.Context, name, providerID string, userData []byte) (*cluster.Node, error) {
	if err := p.client.AttachCloudInit(ctx, name, userData); err != nil {
		return nil, err
	}
	if err := p.client.StartVM(ctx, name); err != nil {
		return nil, err
	}
	addrs, err := p.waitForAddress(ctx, name)
	if err != nil {
		return nil, err
	}
	return &cluster.Node{ID: providerID, Name: name, Addresses: addrs}, nil
}

// waitForAddress polls the guest until it reports at least one IPv4 address or
// the timeout elapses. The daemon discovers the address from the host's DHCP
// leases, so it only appears after the guest has requested DHCP.
func (p *Provider) waitForAddress(ctx context.Context, name string) ([]string, error) {
	deadline := time.Now().Add(addressTimeout)
	for {
		addrs, err := p.client.GetIPAddresses(ctx, name)
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

// cleanup removes a half-created VM, best effort: it force-stops the runner and
// forgets the VM (deleting its working disk and seed). A missing VM is not an
// error on the daemon side.
func (p *Provider) cleanup(ctx context.Context, name string) {
	_ = p.client.StopVM(ctx, name, true)
	_ = p.client.RemoveVM(ctx, name)
}
