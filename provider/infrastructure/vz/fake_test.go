package vz

import (
	"context"
	"fmt"
)

// fakeClient is an in-memory Client for tests. It records the calls the provider
// makes so the tests can assert on them without a Mac or a running daemon.
type fakeClient struct {
	vms   map[string]*VM
	addrs map[string][]string
	nextN int

	created  []CreateVMInput
	attached map[string][]byte
	started  []string
	stopped  []string
	removed  []string

	createErr error
	startErr  error
	attachErr error
	listErr   error
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		vms:      map[string]*VM{},
		addrs:    map[string][]string{},
		attached: map[string][]byte{},
	}
}

func (f *fakeClient) Exists(_ context.Context, name string) (bool, error) {
	_, ok := f.vms[name]
	return ok, nil
}

func (f *fakeClient) GetVM(_ context.Context, name string) (*VM, error) {
	if v, ok := f.vms[name]; ok {
		return v, nil
	}
	return nil, ErrVMNotFound
}

func (f *fakeClient) CreateVM(_ context.Context, in CreateVMInput) (*VM, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, in)
	f.nextN++
	v := &VM{
		Name:        in.Name,
		ID:          fmt.Sprintf("vmid-%d", f.nextN),
		State:       StateStopped,
		CPUCount:    in.CPUCount,
		MemoryBytes: in.MemoryBytes,
		DiskImage:   in.DiskImage,
		Notes:       in.Notes,
	}
	f.vms[in.Name] = v
	return v, nil
}

func (f *fakeClient) StartVM(_ context.Context, name string) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, name)
	if v, ok := f.vms[name]; ok {
		v.State = StateRunning
	}
	return nil
}

func (f *fakeClient) StopVM(_ context.Context, name string, _ bool) error {
	f.stopped = append(f.stopped, name)
	if v, ok := f.vms[name]; ok {
		v.State = StateStopped
	}
	return nil
}

func (f *fakeClient) RemoveVM(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	delete(f.vms, name)
	return nil
}

func (f *fakeClient) AttachCloudInit(_ context.Context, name string, userData []byte) error {
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attached[name] = append([]byte(nil), userData...)
	return nil
}

func (f *fakeClient) GetIPAddresses(_ context.Context, name string) ([]string, error) {
	return f.addrs[name], nil
}

func (f *fakeClient) ListVMs(_ context.Context) ([]*VM, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*VM
	for _, v := range f.vms {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeClient) Close() error { return nil }

// seed registers an already-running VM with the fake, as if the daemon had one.
func (f *fakeClient) seed(name, id, notes string, addrs ...string) *VM {
	v := &VM{Name: name, ID: id, State: StateRunning, Notes: notes}
	f.vms[name] = v
	if len(addrs) > 0 {
		f.addrs[name] = addrs
	}
	return v
}
