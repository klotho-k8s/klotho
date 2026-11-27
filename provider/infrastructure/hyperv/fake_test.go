package hyperv

import (
	"context"
	"fmt"
)

// fakeClient is an in-memory Client for tests.
type fakeClient struct {
	vms      map[string]*VM
	switches []string
	addrs    map[string][]string
	nextGUID int

	created   []CreateInput
	attached  map[string]string // vm name -> iso path
	firmware  map[string]bool   // vm name -> secureBoot
	started   []string
	removed   []string
	createErr error
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		vms:      map[string]*VM{},
		switches: []string{DefaultSwitch},
		addrs:    map[string][]string{},
		attached: map[string]string{},
		firmware: map[string]bool{},
	}
}

func (f *fakeClient) GetVM(_ context.Context, name string) (*VM, error) {
	if vm, ok := f.vms[name]; ok {
		return vm, nil
	}
	return nil, ErrVMNotFound
}

func (f *fakeClient) CreateVM(_ context.Context, in CreateInput) (*VM, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, in)
	f.nextGUID++
	vm := &VM{Name: in.Name, ID: fmt.Sprintf("guid-%d", f.nextGUID), State: StateOff, Notes: in.Notes}
	f.vms[in.Name] = vm
	return vm, nil
}

func (f *fakeClient) AttachISO(_ context.Context, name, isoPath string) error {
	f.attached[name] = isoPath
	return nil
}

func (f *fakeClient) SetFirmware(_ context.Context, name string, secureBoot bool) error {
	f.firmware[name] = secureBoot
	return nil
}

func (f *fakeClient) StartVM(_ context.Context, name string) error {
	f.started = append(f.started, name)
	if vm, ok := f.vms[name]; ok {
		vm.State = StateRunning
	}
	return nil
}

func (f *fakeClient) StopVM(_ context.Context, name string, _ bool) error {
	if vm, ok := f.vms[name]; ok {
		vm.State = StateOff
	}
	return nil
}

func (f *fakeClient) RemoveVM(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	delete(f.vms, name)
	return nil
}

func (f *fakeClient) ListVMs(_ context.Context, prefix string) ([]*VM, error) {
	var out []*VM
	for _, vm := range f.vms {
		if len(vm.Name) >= len(prefix) && vm.Name[:len(prefix)] == prefix {
			out = append(out, vm)
		}
	}
	return out, nil
}

func (f *fakeClient) GetIPAddresses(_ context.Context, name string) ([]string, error) {
	return f.addrs[name], nil
}

func (f *fakeClient) ListSwitches(_ context.Context) ([]string, error) {
	return f.switches, nil
}
