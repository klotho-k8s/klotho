package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cloudinit"
	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// addressTimeout bounds the wait for the guest to report an IPv4 address.
const addressTimeout = 5 * time.Minute

// addressPoll is how often the guest address is polled.
const addressPoll = 5 * time.Second

// Launch boots the bootstrap node as a single Generation 2 Hyper-V VM from the
// configured Linux VHDX with a NoCloud cidata ISO carrying userData, waits for
// an IPv4 address, and returns the node. The Cluster API Hyper-V provider later
// adopts this VM because its name matches the HyperVMachine's spec.vmName and
// it is running.
//
// The VM is created before the ISO is written so the VM's GUID can seed the
// NoCloud meta-data instance-id. The adopted HyperVMachine's provider ID is
// "hyperv://<guid>", and the cluster template sets the kubelet --provider-id to
// "hyperv://{{ ds.meta_data.instance_id }}", so the instance-id must be that
// same GUID for the Node's provider ID to match the Machine's and the
// Machine↔Node nodeRef to complete (Hyper-V runs no cloud controller manager
// to set it otherwise).
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	if len(userData) == 0 {
		return nil, errorf("empty user-data")
	}
	r := p.resolve(spec.Variables)
	if r.Image == "" {
		return nil, errorf("no base image: set hyperv.image or the %s variable", VarImagePath)
	}
	name := VMName(spec.Name)

	if vm, err := p.client.GetVM(ctx, name); err == nil && vm != nil {
		return nil, errorf("bootstrap VM %q already exists; delete the cluster first", name)
	} else if err != nil && !errors.Is(err, ErrVMNotFound) {
		return nil, err
	}

	node, isoPath, err := p.launch(ctx, spec.Name, r, name, userData)
	if err != nil {
		p.cleanup(ctx, name, isoPath)
		return nil, errorf("launching %q: %w", name, err)
	}
	return node, nil
}

// writeISO renders the NoCloud cidata ISO and writes it under the ISO
// directory, returning its path. instanceID becomes the meta-data instance-id,
// which the kubelet --provider-id resolves through
// "{{ ds.meta_data.instance_id }}"; it is the VM's GUID so the Node's provider
// ID matches the adopted HyperVMachine's.
func (p *Provider) writeISO(clusterName string, r resolved, vmName, instanceID string, userData []byte) (string, error) {
	md, err := json.Marshal(struct {
		InstanceID    string `json:"instance-id"`
		LocalHostname string `json:"local-hostname"`
	}{InstanceID: instanceID, LocalHostname: vmName})
	if err != nil {
		return "", errorf("building meta-data: %w", err)
	}
	iso, err := cloudinit.ISO(&cloudinit.Bundle{UserData: userData, MetaData: md})
	if err != nil {
		return "", errorf("building cidata ISO: %w", err)
	}
	dir := r.ISODir
	if dir == "" {
		dir, err = os.MkdirTemp("", "klotho-hyperv-")
		if err != nil {
			return "", errorf("creating ISO directory: %w", err)
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errorf("creating ISO directory %q: %w", dir, err)
	}
	path := filepath.Join(dir, ISOName(clusterName))
	if err := os.WriteFile(path, iso, 0o600); err != nil {
		return "", errorf("writing ISO %q: %w", path, err)
	}
	return path, nil
}

// launch creates the VM, writes and attaches its NoCloud ISO (seeded with the
// VM's GUID as the instance-id), configures, starts the VM and waits for its
// address. It returns the ISO path even on a late failure so the caller can
// clean it up.
func (p *Provider) launch(ctx context.Context, clusterName string, r resolved, name string, userData []byte) (*cluster.Node, string, error) {
	vm, err := p.client.CreateVM(ctx, CreateInput{
		Name:       name,
		Image:      r.Image,
		Switch:     r.Switch,
		VMPath:     r.VMPath,
		MemoryMiB:  r.MemoryMiB,
		CPUs:       r.CPUs,
		Notes:      Tag(clusterName),
		SecureBoot: p.cfg.SecureBoot,
	})
	if err != nil {
		return nil, "", err
	}
	isoPath, err := p.writeISO(clusterName, r, name, vm.ID, userData)
	if err != nil {
		return nil, "", err
	}
	if err := p.client.AttachISO(ctx, name, isoPath); err != nil {
		return nil, isoPath, err
	}
	if err := p.client.SetFirmware(ctx, name, p.cfg.SecureBoot); err != nil {
		return nil, isoPath, err
	}
	if err := p.client.StartVM(ctx, name); err != nil {
		return nil, isoPath, err
	}
	addrs, err := p.waitForAddress(ctx, name)
	if err != nil {
		return nil, isoPath, err
	}
	return &cluster.Node{
		ID:        ProviderIDPrefix + vm.ID,
		Name:      name,
		Addresses: addrs,
	}, isoPath, nil
}

// waitForAddress polls the guest until it reports at least one IPv4 address or
// the timeout elapses.
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

// EndpointFromBootstrap implements infrastructure.EndpointFromBootstrap: the
// Hyper-V control plane endpoint is the bootstrap node's own address, a DHCP
// lease from the virtual switch that is not known until the node boots. It
// returns the node's first IPv4 address, or "" when none is known yet (Create
// then keeps the prepared placeholder endpoint).
func (p *Provider) EndpointFromBootstrap(node *cluster.Node) string {
	if node == nil {
		return ""
	}
	if v4 := ipv4(node.Addresses); len(v4) > 0 {
		return v4[0]
	}
	return ""
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

// cleanup removes a half-created VM and its ISO, best effort.
func (p *Provider) cleanup(ctx context.Context, name, isoPath string) {
	_ = p.client.RemoveVM(ctx, name)
	if isoPath != "" {
		_ = os.Remove(isoPath)
	}
}
