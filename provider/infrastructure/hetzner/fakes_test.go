package hetzner

import (
	"context"
	"net"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// fakeHetzner is an in-memory API for tests. It records the calls that matter
// and serves servers so Launch/Plan/Teardown/Adopt can be exercised without
// Hetzner.
type fakeHetzner struct {
	serverTypes []*hcloud.ServerType
	images      []*hcloud.Image
	locations   []*hcloud.Location
	sshKeys     map[string]*hcloud.SSHKey

	servers map[int64]*hcloud.Server
	nextID  int64

	createOpts *hcloud.ServerCreateOpts
	deleted    []int64
}

func newFakeHetzner() *fakeHetzner {
	return &fakeHetzner{
		nextID:      1001,
		servers:     map[int64]*hcloud.Server{},
		serverTypes: []*hcloud.ServerType{{ID: 22, Name: "cpx21", Description: "CPX21", Cores: 3, Memory: 4, Architecture: hcloud.ArchitectureX86}},
		images:      []*hcloud.Image{{ID: 114690387, Name: "ubuntu-22.04", Description: "Ubuntu 22.04", OSFlavor: "ubuntu", Architecture: hcloud.ArchitectureX86}},
		locations:   []*hcloud.Location{{ID: 1, Name: "fsn1", Description: "Falkenstein DC Park 1", City: "Falkenstein", Country: "DE"}},
		sshKeys:     map[string]*hcloud.SSHKey{"klotho": {ID: 7, Name: "klotho"}},
	}
}

func (f *fakeHetzner) CreateServer(_ context.Context, opts hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, error) {
	f.createOpts = &opts
	id := f.nextID
	f.nextID++
	srv := &hcloud.Server{
		ID:     id,
		Name:   opts.Name,
		Status: hcloud.ServerStatusRunning,
		Labels: opts.Labels,
		PublicNet: hcloud.ServerPublicNet{
			IPv4: hcloud.ServerPublicNetIPv4{IP: net.ParseIP("192.0.2.10")},
			IPv6: hcloud.ServerPublicNetIPv6{IP: net.ParseIP("2001:db8::1")},
		},
	}
	f.servers[id] = srv
	return hcloud.ServerCreateResult{Server: srv}, nil
}

func (f *fakeHetzner) GetServer(_ context.Context, id int64) (*hcloud.Server, error) {
	return f.servers[id], nil
}

func (f *fakeHetzner) ListServers(_ context.Context, selector string) ([]*hcloud.Server, error) {
	key, val := splitSelector(selector)
	var out []*hcloud.Server
	for _, s := range f.servers {
		if key == "" || s.Labels[key] == val {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeHetzner) DeleteServer(_ context.Context, id int64) error {
	if _, ok := f.servers[id]; !ok {
		return hcloud.Error{Code: hcloud.ErrorCodeNotFound, Message: "server not found"}
	}
	f.deleted = append(f.deleted, id)
	delete(f.servers, id)
	return nil
}

func (f *fakeHetzner) GetServerType(_ context.Context, name string) (*hcloud.ServerType, error) {
	for _, t := range f.serverTypes {
		if t.Name == name {
			return t, nil
		}
	}
	return nil, nil
}

func (f *fakeHetzner) GetImage(_ context.Context, name string) (*hcloud.Image, error) {
	for _, img := range f.images {
		if img.Name == name {
			return img, nil
		}
	}
	return nil, nil
}

func (f *fakeHetzner) GetLocation(_ context.Context, name string) (*hcloud.Location, error) {
	for _, l := range f.locations {
		if l.Name == name {
			return l, nil
		}
	}
	return nil, nil
}

func (f *fakeHetzner) GetSSHKey(_ context.Context, name string) (*hcloud.SSHKey, error) {
	return f.sshKeys[name], nil
}

func (f *fakeHetzner) ListServerTypes(_ context.Context) ([]*hcloud.ServerType, error) {
	return f.serverTypes, nil
}

func (f *fakeHetzner) ListImages(_ context.Context) ([]*hcloud.Image, error) {
	return f.images, nil
}

func (f *fakeHetzner) ListLocations(_ context.Context) ([]*hcloud.Location, error) {
	return f.locations, nil
}

// seedServer adds a running server with the given id, name and labels.
func (f *fakeHetzner) seedServer(id int64, name string, labels map[string]string) {
	f.servers[id] = &hcloud.Server{
		ID:     id,
		Name:   name,
		Status: hcloud.ServerStatusRunning,
		Labels: labels,
		PublicNet: hcloud.ServerPublicNet{
			IPv4: hcloud.ServerPublicNetIPv4{IP: net.ParseIP("192.0.2.10")},
		},
	}
}

// splitSelector parses the "key=value" label selector the provider builds.
func splitSelector(selector string) (string, string) {
	for i := 0; i < len(selector); i++ {
		if selector[i] == '=' {
			return selector[:i], selector[i+1:]
		}
	}
	return "", ""
}
