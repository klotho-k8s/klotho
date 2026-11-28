package vsphere

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Guestinfo keys the VMware cloud-init datasource reads. klotho delivers the
// bootstrap user-data and metadata through them, base64-encoded, the same
// channel CAPV itself uses, so the adopted VM booted the identical way.
const (
	GuestinfoUserData         = "guestinfo.userdata"
	GuestinfoUserDataEncoding = "guestinfo.userdata.encoding"
	GuestinfoMetadata         = "guestinfo.metadata"
	GuestinfoMetadataEncoding = "guestinfo.metadata.encoding"
)

// metaData is the minimal cloud-init metadata document. instance-id must be the
// BIOS UUID so the kubelet --provider-id "vsphere://{{ ds.meta_data.instance_id
// }}" resolves to the adopted VSphereMachine's provider ID.
type metaData struct {
	InstanceID    string `json:"instance-id"`
	LocalHostname string `json:"local-hostname"`
}

// Launch boots the bootstrap node as a single vSphere VM: it clones the
// template (powered off), writes the guestinfo user-data and metadata (seeded
// with the VM's BIOS UUID as instance-id), powers the VM on, and waits for an
// IPv4 address. CAPV later adopts this VM because klotho pre-sets the
// VSphereMachine's providerID to "vsphere://<uuid>" (see Adopt).
//
// The returned node's ID is "vsphere://<uuid>" and its Name is the VM name. On
// any failure after the clone, the VM is destroyed on a best-effort basis and
// the original error is returned.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	if len(userData) == 0 {
		return nil, errorf("empty user-data")
	}
	r := p.resolve(spec.Variables)
	if r.Template == "" {
		return nil, errorf("no template: set vsphere.template or the %s variable", VarTemplate)
	}
	name := VMName(spec.Name)

	if _, err := p.client.FindVM(ctx, name); err == nil {
		return nil, errorf("bootstrap VM %q already exists; delete the cluster first", name)
	} else if !errors.Is(err, ErrVMNotFound) {
		return nil, err
	}

	vm, err := p.client.CloneVM(ctx, CloneInput{
		Name:         name,
		Template:     r.Template,
		Datacenter:   r.Datacenter,
		Datastore:    r.Datastore,
		Folder:       r.Folder,
		ResourcePool: r.ResourcePool,
		Network:      r.Network,
		CloneMode:    r.CloneMode,
		NumCPUs:      int32(r.NumCPUs),
		MemoryMiB:    int64(r.MemoryMiB),
		DiskGiB:      int32(r.DiskGiB),
		Annotation:   Tag(spec.Name),
	})
	if err != nil {
		return nil, err
	}
	if vm.UUID == "" {
		p.cleanup(ctx, name)
		return nil, errorf("cloned VM %q has no BIOS UUID", name)
	}

	guestinfo, err := buildGuestinfo(vm.UUID, name, userData)
	if err != nil {
		p.cleanup(ctx, name)
		return nil, err
	}
	if err := p.client.SetGuestinfo(ctx, name, guestinfo); err != nil {
		p.cleanup(ctx, name)
		return nil, err
	}
	if err := p.client.PowerOn(ctx, name); err != nil {
		p.cleanup(ctx, name)
		return nil, err
	}
	addrs, err := p.client.WaitForIP(ctx, name)
	if err != nil {
		p.cleanup(ctx, name)
		return nil, errorf("launching %q: %w", name, err)
	}
	return &cluster.Node{ID: ProviderID(vm.UUID), Name: name, Addresses: addrs}, nil
}

// buildGuestinfo renders the guestinfo ExtraConfig carrying userData and the
// cloud-init metadata (instance-id = uuid, local-hostname = name), both
// base64-encoded with their encoding keys set.
func buildGuestinfo(uuid, name string, userData []byte) (map[string]string, error) {
	if len(userData) == 0 {
		return nil, errorf("empty user-data")
	}
	if uuid == "" {
		return nil, errorf("empty instance-id (BIOS UUID)")
	}
	md, err := json.Marshal(metaData{InstanceID: uuid, LocalHostname: name})
	if err != nil {
		return nil, errorf("building metadata: %w", err)
	}
	return map[string]string{
		GuestinfoUserData:         base64.StdEncoding.EncodeToString(userData),
		GuestinfoUserDataEncoding: "base64",
		GuestinfoMetadata:         base64.StdEncoding.EncodeToString(md),
		GuestinfoMetadataEncoding: "base64",
	}, nil
}

// cleanup removes a half-created VM, best effort.
func (p *Provider) cleanup(ctx context.Context, name string) {
	_ = p.client.DestroyVM(context.WithoutCancel(ctx), name)
}
