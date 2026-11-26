package azure

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Launch boots the bootstrap node as a single Azure VM carrying klotho's
// cloud-init as customData, named MachineName(cluster) in the cluster resource
// group so CAPZ adopts it. It creates the VM's NIC (associating the public IP
// allocated in Prepare), creates the VM, waits for it to provision, and returns
// the node with its provider ID and addresses.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	subnetID, err := p.api.SubnetID(ctx, p.cfg.ResourceGroup, p.cfg.VNet, p.cfg.Subnet)
	if err != nil {
		return nil, errorf("resolving subnet %s/%s/%s: %w", p.cfg.ResourceGroup, p.cfg.VNet, p.cfg.Subnet, err)
	}

	// The public IP was allocated in Prepare; re-find it so Launch can run on a
	// resumed create without re-allocating.
	pip, err := p.api.EnsurePublicIP(ctx, PublicIPParams{
		ResourceGroup: p.cfg.ResourceGroup,
		Location:      p.cfg.Location,
		Name:          publicIPName(spec.Name),
		Tags:          tags(spec.Name),
	})
	if err != nil {
		return nil, errorf("resolving the public IP: %w", err)
	}

	nicID, err := p.api.CreateNIC(ctx, NICParams{
		ResourceGroup: p.cfg.ResourceGroup,
		Location:      p.cfg.Location,
		Name:          nicName(spec.Name),
		SubnetID:      subnetID,
		PublicIPID:    pip.ID,
		Tags:          tags(spec.Name),
	})
	if err != nil {
		return nil, errorf("creating the NIC: %w", err)
	}

	vmParams := VMParams{
		ResourceGroup: p.cfg.ResourceGroup,
		Location:      p.cfg.Location,
		Name:          MachineName(spec.Name),
		Size:          p.vmSize(),
		AdminUsername: p.adminUsername(),
		Image:         p.image(),
		NICID:         nicID,
		CustomData:    userData,
		Spot:          p.cfg.Spot,
		Tags:          tags(spec.Name),
	}
	if p.cfg.SSHPublicKey != "" {
		vmParams.SSHPublicKey = p.cfg.SSHPublicKey
	} else {
		// Azure requires either an SSH key or an admin password. klotho reaches
		// the node through cloud-init, so a strong random password is enough.
		pass, err := randomPassword()
		if err != nil {
			return nil, err
		}
		vmParams.AdminPassword = pass
	}

	vm, err := p.api.CreateVM(ctx, vmParams)
	if err != nil {
		return nil, errorf("creating the bootstrap VM: %w", err)
	}

	running, err := p.waitRunning(ctx, vm.Name, vm)
	if err != nil {
		return nil, err
	}

	node := &cluster.Node{
		ID:   providerID(running.ID),
		Name: MachineName(spec.Name),
	}
	node.Addresses = append(node.Addresses, running.PublicIPs...)
	node.Addresses = append(node.Addresses, running.PrivateIPs...)
	// The endpoint public IP is always an address of node zero, even if the VM
	// view has not surfaced it yet.
	if pip.Address != "" && !contains(node.Addresses, pip.Address) {
		node.Addresses = append(node.Addresses, pip.Address)
	}
	return node, nil
}

// waitRunning polls until the VM has provisioned, returning its resource view.
// The initial view from CreateVM is used when it already shows success.
func (p *Provider) waitRunning(ctx context.Context, name string, initial VM) (*VM, error) {
	if provisioned(initial) {
		return &initial, nil
	}
	deadline := time.Now().Add(p.launchWait)
	for {
		vm, err := p.api.GetVM(ctx, p.cfg.ResourceGroup, name)
		if err != nil {
			return nil, errorf("describing VM %s: %w", name, err)
		}
		if vm != nil {
			if provisioned(*vm) {
				return vm, nil
			}
			if strings.EqualFold(vm.ProvisioningState, "Failed") {
				return nil, errorf("VM %s entered provisioning state %q", name, vm.ProvisioningState)
			}
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for VM %s to provision", name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

// provisioned reports whether a VM view shows a successful provisioning state.
func provisioned(vm VM) bool {
	return strings.EqualFold(vm.ProvisioningState, "Succeeded")
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// randomPassword returns a strong random admin password that meets Azure's
// Linux VM complexity rules (12-72 chars, three of four character classes).
// klotho reaches the node through cloud-init, so the password is never used.
func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", errorf("generating an admin password: %w", err)
	}
	return "Aa1!" + base64.RawURLEncoding.EncodeToString(b), nil
}
