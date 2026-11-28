package openstack

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
)

// fakeClient is an in-memory ComputeAPI for tests. It records the calls that
// matter and serves servers and floating IPs so Prepare/Launch/Plan/Teardown
// can be exercised without OpenStack.
type fakeClient struct {
	servers map[string]*Server     // by ID
	fips    map[string]*FloatingIP // by ID

	created      *CreateServerOpts
	deleted      []string          // server IDs
	delFIPs      []string          // floating IP IDs
	associations map[string]string // floating IP ID -> server ID
	allocated    []string          // floating IP IDs

	nextServer int
	nextFIP    int

	fipAddr       string
	noNetwork     bool
	missingFlavor bool
	missingImage  bool
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		servers:      map[string]*Server{},
		fips:         map[string]*FloatingIP{},
		associations: map[string]string{},
		fipAddr:      "203.0.113.20",
	}
}

func notFoundErr() error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: 404}
}

func (f *fakeClient) CreateServer(_ context.Context, opts CreateServerOpts) (*Server, error) {
	f.created = &opts
	f.nextServer++
	srv := &Server{
		ID:        fmt.Sprintf("srv-%d", f.nextServer),
		Name:      opts.Name,
		Status:    "ACTIVE",
		Addresses: []string{"10.0.0.5"},
		Tags:      append([]string{}, opts.Tags...),
	}
	f.servers[srv.ID] = srv
	return &Server{ID: srv.ID, Name: srv.Name, Status: srv.Status}, nil
}

func (f *fakeClient) GetServer(_ context.Context, id string) (*Server, error) {
	if s, ok := f.servers[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, notFoundErr()
}

func (f *fakeClient) DeleteServer(_ context.Context, id string) error {
	if _, ok := f.servers[id]; !ok {
		return notFoundErr()
	}
	f.deleted = append(f.deleted, id)
	delete(f.servers, id)
	return nil
}

func (f *fakeClient) ListServersByTag(_ context.Context, tag string) ([]Server, error) {
	var out []Server
	for _, s := range f.servers {
		for _, t := range s.Tags {
			if t == tag {
				out = append(out, *s)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeClient) FlavorIDByName(_ context.Context, name string) (string, error) {
	if f.missingFlavor {
		return "", errorf("flavor %q not found", name)
	}
	return "flavor-" + name, nil
}

func (f *fakeClient) ImageIDByName(_ context.Context, name string) (string, error) {
	if f.missingImage {
		return "", errorf("image %q not found", name)
	}
	return "image-" + name, nil
}

func (f *fakeClient) NetworkIDByName(_ context.Context, name string) (string, error) {
	if f.noNetwork {
		return "", errorf("network %q not found", name)
	}
	return "net-" + name, nil
}

func (f *fakeClient) AllocateFloatingIP(_ context.Context, externalNetworkID, _ string) (*FloatingIP, error) {
	f.nextFIP++
	fip := &FloatingIP{ID: fmt.Sprintf("fip-%d", f.nextFIP), Address: f.fipAddr}
	f.fips[fip.ID] = fip
	f.allocated = append(f.allocated, fip.ID)
	return &FloatingIP{ID: fip.ID, Address: fip.Address}, nil
}

func (f *fakeClient) GetFloatingIP(_ context.Context, id string) (*FloatingIP, error) {
	if fip, ok := f.fips[id]; ok {
		cp := *fip
		return &cp, nil
	}
	return nil, notFoundErr()
}

func (f *fakeClient) AssociateFloatingIP(_ context.Context, floatingIPID, serverID string) error {
	if _, ok := f.fips[floatingIPID]; !ok {
		return notFoundErr()
	}
	f.associations[floatingIPID] = serverID
	return nil
}

func (f *fakeClient) DeleteFloatingIP(_ context.Context, id string) error {
	if _, ok := f.fips[id]; !ok {
		return notFoundErr()
	}
	f.delFIPs = append(f.delFIPs, id)
	delete(f.fips, id)
	return nil
}

// seedServer adds a server with the given id, name and tags.
func (f *fakeClient) seedServer(id, name string, tags []string) {
	f.servers[id] = &Server{ID: id, Name: name, Status: "ACTIVE", Tags: tags}
}

// seedFIP adds a floating IP.
func (f *fakeClient) seedFIP(id, addr string) {
	f.fips[id] = &FloatingIP{ID: id, Address: addr}
}
