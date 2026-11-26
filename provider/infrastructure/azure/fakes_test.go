package azure

import (
	"context"
	"strings"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// fakeAzure is an in-memory API for tests. It records the calls that
// matter and serves VMs, public IPs and cluster resources so
// Prepare/Launch/Plan/Teardown can be exercised without Azure.
type fakeAzure struct {
	subnetID string
	subnets  []infrastructure.Item
	vnets    []infrastructure.Item

	// returned/recorded state.
	vms       map[string]*VM
	publicIPs map[string]PublicIP
	nextVMID  func(rg, name string) string

	createVMInput  *VMParams
	createNICInput *NICParams
	publicIPInput  *PublicIPParams

	clusterResources []cluster.Resource

	deletedVMs  []string
	deletedNICs []string
	deletedPIPs []string

	subnetErr error
}

func newFakeAzure() *fakeAzure {
	return &fakeAzure{
		subnetID:  "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/virtualNetworks/vnet-1/subnets/subnet-1",
		vms:       map[string]*VM{},
		publicIPs: map[string]PublicIP{},
	}
}

func (f *fakeAzure) SubnetID(_ context.Context, _, _, _ string) (string, error) {
	if f.subnetErr != nil {
		return "", f.subnetErr
	}
	return f.subnetID, nil
}

func (f *fakeAzure) EnsurePublicIP(_ context.Context, p PublicIPParams) (PublicIP, error) {
	in := p
	f.publicIPInput = &in
	if existing, ok := f.publicIPs[p.Name]; ok {
		return existing, nil
	}
	pip := PublicIP{
		Name:    p.Name,
		ID:      "/subscriptions/sub-1/resourceGroups/" + p.ResourceGroup + "/providers/Microsoft.Network/publicIPAddresses/" + p.Name,
		Address: "20.0.0.5",
	}
	f.publicIPs[p.Name] = pip
	return pip, nil
}

func (f *fakeAzure) CreateNIC(_ context.Context, p NICParams) (string, error) {
	in := p
	f.createNICInput = &in
	return "/subscriptions/sub-1/resourceGroups/" + p.ResourceGroup + "/providers/Microsoft.Network/networkInterfaces/" + p.Name, nil
}

func (f *fakeAzure) CreateVM(_ context.Context, p VMParams) (VM, error) {
	in := p
	f.createVMInput = &in
	id := "/subscriptions/sub-1/resourceGroups/" + p.ResourceGroup + "/providers/Microsoft.Compute/virtualMachines/" + p.Name
	if f.nextVMID != nil {
		id = f.nextVMID(p.ResourceGroup, p.Name)
	}
	vm := &VM{
		Name:              p.Name,
		ID:                id,
		ProvisioningState: "Succeeded",
		PrivateIPs:        []string{"10.0.0.4"},
	}
	f.vms[p.Name] = vm
	return *vm, nil
}

func (f *fakeAzure) GetVM(_ context.Context, _, name string) (*VM, error) {
	if vm, ok := f.vms[name]; ok {
		return vm, nil
	}
	return nil, nil
}

func (f *fakeAzure) ListClusterResources(_ context.Context, _, _ string) ([]cluster.Resource, error) {
	return f.clusterResources, nil
}

func (f *fakeAzure) DeleteVM(_ context.Context, _, name string) error {
	f.deletedVMs = append(f.deletedVMs, name)
	delete(f.vms, name)
	return nil
}

func (f *fakeAzure) DeleteNIC(_ context.Context, _, name string) error {
	f.deletedNICs = append(f.deletedNICs, name)
	return nil
}

func (f *fakeAzure) DeletePublicIP(_ context.Context, _, name string) error {
	f.deletedPIPs = append(f.deletedPIPs, name)
	return nil
}

func (f *fakeAzure) ListSubnets(_ context.Context, _, _ string) ([]infrastructure.Item, error) {
	return f.subnets, nil
}

func (f *fakeAzure) ListVirtualNetworks(_ context.Context, _ string) ([]infrastructure.Item, error) {
	return f.vnets, nil
}

// customDataString returns the decoded... (the fake stores raw CustomData, not
// base64; the client base64-encodes, not the provider).
func customDataString(p *VMParams) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(string(p.CustomData))
}
