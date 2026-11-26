package gcp

import (
	"context"
	"fmt"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// fakeCompute is an in-memory ComputeAPI for tests. It records the calls that
// matter and serves instances and addresses so Launch/Prepare/Plan/Teardown can
// be exercised without GCP.
type fakeCompute struct {
	instances map[string]*compute.Instance // by name
	addresses map[string]*compute.Address  // by name

	inserted  *compute.Instance
	deleted   []string // instance names
	delAddrs  []string // address names
	nextAddr  int
	zone      string
	natIP     string
	internal  string
	noNetwork bool
	noImage   bool
}

func newFakeCompute() *fakeCompute {
	return &fakeCompute{
		instances: map[string]*compute.Instance{},
		addresses: map[string]*compute.Address{},
		zone:      "us-central1-a",
		natIP:     "203.0.113.10",
		internal:  "10.0.0.10",
	}
}

func notFoundErr() error { return &googleapi.Error{Code: 404, Message: "not found"} }

func (f *fakeCompute) InsertInstance(_ context.Context, _, zone string, inst *compute.Instance) (*compute.Operation, error) {
	f.inserted = inst
	stored := *inst
	stored.Status = "RUNNING"
	stored.Zone = "https://www.googleapis.com/compute/v1/projects/p/zones/" + zone
	stored.NetworkInterfaces = []*compute.NetworkInterface{{
		NetworkIP:     f.internal,
		AccessConfigs: []*compute.AccessConfig{{NatIP: f.natIP}},
	}}
	f.instances[inst.Name] = &stored
	return &compute.Operation{Name: "op-insert-" + inst.Name}, nil
}

func (f *fakeCompute) GetInstance(_ context.Context, _, _, name string) (*compute.Instance, error) {
	if inst, ok := f.instances[name]; ok {
		return inst, nil
	}
	return nil, notFoundErr()
}

func (f *fakeCompute) DeleteInstance(_ context.Context, _, _, name string) (*compute.Operation, error) {
	if _, ok := f.instances[name]; !ok {
		return nil, notFoundErr()
	}
	f.deleted = append(f.deleted, name)
	delete(f.instances, name)
	return &compute.Operation{Name: "op-delete-" + name}, nil
}

func (f *fakeCompute) ListInstancesByLabel(_ context.Context, _, label, value string) ([]*compute.Instance, error) {
	var out []*compute.Instance
	for _, inst := range f.instances {
		if inst.Labels[label] == value {
			out = append(out, inst)
		}
	}
	return out, nil
}

func (f *fakeCompute) WaitZoneOperation(_ context.Context, _, _, op string) (*compute.Operation, error) {
	return &compute.Operation{Name: op, Status: "DONE"}, nil
}

func (f *fakeCompute) InsertGlobalAddress(_ context.Context, _ string, addr *compute.Address) (*compute.Operation, error) {
	f.nextAddr++
	stored := *addr
	stored.Address = fmt.Sprintf("34.120.0.%d", f.nextAddr)
	f.addresses[addr.Name] = &stored
	return &compute.Operation{Name: "op-addr-" + addr.Name}, nil
}

func (f *fakeCompute) GetGlobalAddress(_ context.Context, _, name string) (*compute.Address, error) {
	if a, ok := f.addresses[name]; ok {
		return a, nil
	}
	return nil, notFoundErr()
}

func (f *fakeCompute) DeleteGlobalAddress(_ context.Context, _, name string) (*compute.Operation, error) {
	if _, ok := f.addresses[name]; !ok {
		return nil, notFoundErr()
	}
	f.delAddrs = append(f.delAddrs, name)
	delete(f.addresses, name)
	return &compute.Operation{Name: "op-deladdr-" + name}, nil
}

func (f *fakeCompute) WaitGlobalOperation(_ context.Context, _, op string) (*compute.Operation, error) {
	return &compute.Operation{Name: op, Status: "DONE"}, nil
}

func (f *fakeCompute) GetImageFromFamily(_ context.Context, _, family string) (*compute.Image, error) {
	if f.noImage {
		return nil, notFoundErr()
	}
	return &compute.Image{Name: family + "-v20260101", Family: family}, nil
}

func (f *fakeCompute) GetImage(_ context.Context, _, image string) (*compute.Image, error) {
	if f.noImage {
		return nil, notFoundErr()
	}
	return &compute.Image{Name: image}, nil
}

func (f *fakeCompute) GetNetwork(_ context.Context, _, network string) (*compute.Network, error) {
	if f.noNetwork {
		return nil, notFoundErr()
	}
	return &compute.Network{Name: network}, nil
}

func (f *fakeCompute) GetSubnetwork(_ context.Context, _, _, subnet string) (*compute.Subnetwork, error) {
	if f.noNetwork {
		return nil, notFoundErr()
	}
	return &compute.Subnetwork{Name: subnet}, nil
}

// seedInstance adds a running instance with the given name, zone and labels.
func (f *fakeCompute) seedInstance(name, zone string, labels map[string]string) {
	f.instances[name] = &compute.Instance{
		Name:   name,
		Status: "RUNNING",
		Zone:   "https://www.googleapis.com/compute/v1/projects/p/zones/" + zone,
		Labels: labels,
	}
}

// seedAddress adds a reserved global address.
func (f *fakeCompute) seedAddress(name, ip string, labels map[string]string) {
	f.addresses[name] = &compute.Address{Name: name, Address: ip, Labels: labels}
}
