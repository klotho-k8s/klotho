package linode

import (
	"context"
	"fmt"
	"net"

	"github.com/linode/linodego/v2"
)

// fakeLinode is an in-memory LinodeAPI for tests. It records the calls that
// matter and serves instances so Launch/Plan/Teardown/Adopt can be exercised
// without Linode.
type fakeLinode struct {
	regions []linodego.Region
	types   []linodego.LinodeType
	images  []linodego.Image

	instances map[int]*linodego.Instance
	nextID    int

	createOpts *linodego.InstanceCreateOptions
	deleted    []int
}

func newFakeLinode() *fakeLinode {
	return &fakeLinode{
		instances: map[int]*linodego.Instance{},
		nextID:    1001,
		regions:   []linodego.Region{{ID: "us-ord", Label: "Chicago, IL", Country: "us", Capabilities: []string{"Linodes", "Metadata"}}},
		types:     []linodego.LinodeType{{ID: "g6-standard-2", Label: "Linode 4GB", VCPUs: 2, Memory: 4096, Price: &linodego.LinodePrice{Hourly: 0.036, Monthly: 24}}},
		images:    []linodego.Image{{ID: "linode/ubuntu22.04", Label: "Ubuntu 22.04", Capabilities: []string{"cloud-init"}}},
	}
}

func (f *fakeLinode) CreateInstance(_ context.Context, opts linodego.InstanceCreateOptions) (*linodego.Instance, error) {
	f.createOpts = &opts
	id := f.nextID
	f.nextID++
	inst := &linodego.Instance{
		ID:     id,
		Label:  opts.Label,
		Region: opts.Region,
		Type:   opts.Type,
		Image:  opts.Image,
		Status: linodego.InstanceRunning,
		Tags:   opts.Tags,
		IPv4:   []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("192.168.139.10")},
	}
	f.instances[id] = inst
	return inst, nil
}

func (f *fakeLinode) GetInstance(_ context.Context, id int) (*linodego.Instance, error) {
	if inst, ok := f.instances[id]; ok {
		return inst, nil
	}
	return nil, &linodego.Error{Code: 404, Message: "Not found"}
}

func (f *fakeLinode) ListInstances(_ context.Context, _ *linodego.ListOptions) ([]linodego.Instance, error) {
	out := make([]linodego.Instance, 0, len(f.instances))
	for _, inst := range f.instances {
		out = append(out, *inst)
	}
	return out, nil
}

func (f *fakeLinode) DeleteInstance(_ context.Context, id int) error {
	if _, ok := f.instances[id]; !ok {
		return &linodego.Error{Code: 404, Message: "Not found"}
	}
	f.deleted = append(f.deleted, id)
	delete(f.instances, id)
	return nil
}

func (f *fakeLinode) GetRegion(_ context.Context, id string) (*linodego.Region, error) {
	for i := range f.regions {
		if f.regions[i].ID == id {
			return &f.regions[i], nil
		}
	}
	return nil, &linodego.Error{Code: 404, Message: fmt.Sprintf("region %q not found", id)}
}

func (f *fakeLinode) GetType(_ context.Context, id string) (*linodego.LinodeType, error) {
	for i := range f.types {
		if f.types[i].ID == id {
			return &f.types[i], nil
		}
	}
	return nil, &linodego.Error{Code: 404, Message: fmt.Sprintf("type %q not found", id)}
}

func (f *fakeLinode) GetImage(_ context.Context, id string) (*linodego.Image, error) {
	for i := range f.images {
		if f.images[i].ID == id {
			return &f.images[i], nil
		}
	}
	return nil, &linodego.Error{Code: 404, Message: fmt.Sprintf("image %q not found", id)}
}

func (f *fakeLinode) ListRegions(_ context.Context, _ *linodego.ListOptions) ([]linodego.Region, error) {
	return f.regions, nil
}

func (f *fakeLinode) ListTypes(_ context.Context, _ *linodego.ListOptions) ([]linodego.LinodeType, error) {
	return f.types, nil
}

func (f *fakeLinode) ListImages(_ context.Context, _ *linodego.ListOptions) ([]linodego.Image, error) {
	return f.images, nil
}

// seedInstance adds a running instance with the given id, label and tags.
func (f *fakeLinode) seedInstance(id int, label string, tags []string) {
	f.instances[id] = &linodego.Instance{
		ID:     id,
		Label:  label,
		Status: linodego.InstanceRunning,
		Tags:   tags,
		IPv4:   []net.IP{net.ParseIP("192.0.2.10")},
	}
}
