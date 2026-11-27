package libvirt

import (
	"context"
	"fmt"
	"strings"
)

// fakeClient is an in-memory Client for tests.
type fakeClient struct {
	host     string
	domains  map[string]*Domain
	networks []string
	pools    []string
	volumes  map[string]bool // "pool/vol" -> exists
	addrs    map[string][]string
	nextN    int

	created   []CreateDomainInput
	started   []string
	destroyed []string
	undefined []string

	createErr error
	startErr  error
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		host:     "localhost",
		domains:  map[string]*Domain{},
		networks: []string{DefaultNetwork},
		pools:    []string{DefaultStoragePool},
		volumes:  map[string]bool{"default/base.qcow2": true},
		addrs:    map[string][]string{},
	}
}

func (f *fakeClient) Host() string { return f.host }

func (f *fakeClient) GetDomain(_ context.Context, name string) (*Domain, error) {
	if d, ok := f.domains[name]; ok {
		return d, nil
	}
	return nil, ErrDomainNotFound
}

func (f *fakeClient) CreateDomain(_ context.Context, in CreateDomainInput) (*Domain, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, in)
	uuid := in.UUID
	if uuid == "" {
		f.nextN++
		uuid = fmt.Sprintf("uuid-%d", f.nextN)
	}
	d := &Domain{Name: in.Name, UUID: uuid, State: StateShutoff, Description: in.Description}
	f.domains[in.Name] = d
	return d, nil
}

func (f *fakeClient) StartDomain(_ context.Context, name string) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, name)
	if d, ok := f.domains[name]; ok {
		d.State = StateRunning
	}
	return nil
}

func (f *fakeClient) DestroyDomain(_ context.Context, name string) error {
	f.destroyed = append(f.destroyed, name)
	if d, ok := f.domains[name]; ok {
		d.State = StateShutoff
	}
	return nil
}

func (f *fakeClient) UndefineDomain(_ context.Context, name string) error {
	f.undefined = append(f.undefined, name)
	delete(f.domains, name)
	return nil
}

func (f *fakeClient) GetDomainIPs(_ context.Context, name string) ([]string, error) {
	return f.addrs[name], nil
}

func (f *fakeClient) ListDomains(_ context.Context, prefix string) ([]*Domain, error) {
	var out []*Domain
	for _, d := range f.domains {
		if strings.HasPrefix(d.Name, prefix) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeClient) ListNetworks(_ context.Context) ([]string, error) {
	return append([]string(nil), f.networks...), nil
}

func (f *fakeClient) ListStoragePools(_ context.Context) ([]string, error) {
	return append([]string(nil), f.pools...), nil
}

func (f *fakeClient) VolumeExists(_ context.Context, pool, vol string) (bool, error) {
	return f.volumes[pool+"/"+vol], nil
}

func (f *fakeClient) Close() error { return nil }
