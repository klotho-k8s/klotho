package vsphere

import (
	"context"
	"strings"
)

// fakeClient is an in-memory Client for tests.
type fakeClient struct {
	vms        map[string]*VM
	templates  map[string]bool
	networks   []string
	datastores []string
	nextUUID   int

	cloned    []CloneInput
	guestinfo map[string]map[string]string
	poweredOn []string
	destroyed []string

	cloneErr error
	ipErr    error
	ips      []string
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		vms:        map[string]*VM{},
		templates:  map[string]bool{"ubuntu-2404-template": true},
		networks:   []string{"VM Network"},
		datastores: []string{"datastore1"},
		guestinfo:  map[string]map[string]string{},
		ips:        []string{"192.168.1.50"},
	}
}

func (f *fakeClient) FindVM(_ context.Context, name string) (*VM, error) {
	if v, ok := f.vms[name]; ok {
		return v, nil
	}
	return nil, ErrVMNotFound
}

func (f *fakeClient) CloneVM(_ context.Context, in CloneInput) (*VM, error) {
	if f.cloneErr != nil {
		return nil, f.cloneErr
	}
	f.cloned = append(f.cloned, in)
	f.nextUUID++
	uuid := fakeUUID(f.nextUUID)
	v := &VM{Name: in.Name, UUID: uuid, Annotation: in.Annotation, PoweredOn: false}
	f.vms[in.Name] = v
	return v, nil
}

func (f *fakeClient) SetGuestinfo(_ context.Context, name string, data map[string]string) error {
	f.guestinfo[name] = data
	return nil
}

func (f *fakeClient) PowerOn(_ context.Context, name string) error {
	f.poweredOn = append(f.poweredOn, name)
	if v, ok := f.vms[name]; ok {
		v.PoweredOn = true
	}
	return nil
}

func (f *fakeClient) WaitForIP(_ context.Context, name string) ([]string, error) {
	if f.ipErr != nil {
		return nil, f.ipErr
	}
	if v, ok := f.vms[name]; ok {
		v.Addresses = append([]string(nil), f.ips...)
	}
	return append([]string(nil), f.ips...), nil
}

func (f *fakeClient) DestroyVM(_ context.Context, name string) error {
	f.destroyed = append(f.destroyed, name)
	delete(f.vms, name)
	return nil
}

func (f *fakeClient) ListVMs(_ context.Context, prefix string) ([]*VM, error) {
	var out []*VM
	for _, v := range f.vms {
		if prefix == "" || strings.HasPrefix(v.Name, prefix) {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeClient) TemplateExists(_ context.Context, name string) (bool, error) {
	return f.templates[name], nil
}

func (f *fakeClient) ListNetworks(_ context.Context) ([]string, error) {
	return append([]string(nil), f.networks...), nil
}

func (f *fakeClient) ListDatastores(_ context.Context) ([]string, error) {
	return append([]string(nil), f.datastores...), nil
}

func (f *fakeClient) ListTemplates(_ context.Context) ([]string, error) {
	var out []string
	for t := range f.templates {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeClient) Close() error { return nil }

// fakeUUID returns a deterministic canonical UUID for test n.
func fakeUUID(n int) string {
	return "423f1a2b-0000-4000-8000-00000000000" + string(rune('0'+n%10))
}
