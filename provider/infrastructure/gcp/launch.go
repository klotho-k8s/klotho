package gcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// metadataKeyUserData is the GCE instance metadata key CAPG delivers cloud-init
// through (cloud/services/compute/instances/reconcile.go). klotho boots node
// zero with the same key so the adopted instance used the identical path.
const metadataKeyUserData = "user-data"

// metadataKeySSHKeys is the GCE metadata key for project/instance SSH keys,
// "<user>:<public-key>" per line.
const metadataKeySSHKeys = "ssh-keys"

// Launch boots the bootstrap node as a single Compute Engine instance named
// MachineName(cluster) — the name CAPG derives the instance name from — carrying
// klotho's cloud-init as the "user-data" metadata item and klotho's ownership
// label, so CAPG's name-keyed Instances.Get adopts it. It waits for the create
// operation, reads the instance, and returns the node.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	name := MachineName(spec.Name)
	zone := p.cfg.Zone
	userDataStr := string(userData)

	inst := &compute.Instance{
		Name:        name,
		MachineType: fmt.Sprintf("zones/%s/machineTypes/%s", zone, p.machineType()),
		Labels:      clusterLabels(spec.Name),
		Disks: []*compute.AttachedDisk{{
			Boot:       true,
			AutoDelete: true,
			InitializeParams: &compute.AttachedDiskInitializeParams{
				SourceImage: p.imageURL(),
				DiskSizeGb:  p.diskSizeGB(),
				DiskType:    fmt.Sprintf("zones/%s/diskTypes/%s", zone, p.diskType()),
			},
		}},
		NetworkInterfaces: []*compute.NetworkInterface{p.networkInterface()},
		Metadata:          p.metadata(userDataStr),
	}
	if len(p.cfg.NetworkTags) > 0 {
		inst.Tags = &compute.Tags{Items: p.cfg.NetworkTags}
	}
	if p.cfg.Preemptible {
		// Preemptible is the long-standing Spot flag; it is accepted on its own.
		// (Setting ProvisioningModel alongside it can be rejected as conflicting.)
		inst.Scheduling = &compute.Scheduling{
			Preemptible:      true,
			AutomaticRestart: ptr(false),
		}
	}
	if p.cfg.ServiceAccount != "" {
		inst.ServiceAccounts = []*compute.ServiceAccount{{
			Email:  p.cfg.ServiceAccount,
			Scopes: []string{compute.CloudPlatformScope},
		}}
	}

	op, err := p.compute.InsertInstance(ctx, p.cfg.Project, zone, inst)
	if err != nil {
		return nil, errorf("launching the bootstrap instance: %w", err)
	}
	if _, err := p.compute.WaitZoneOperation(ctx, p.cfg.Project, zone, op.Name); err != nil {
		return nil, errorf("waiting for instance %q to be created: %w", name, err)
	}

	running, err := p.waitRunning(ctx, name)
	if err != nil {
		return nil, err
	}

	node := &cluster.Node{
		ID:   providerID(p.cfg.Project, zone, name),
		Name: name,
	}
	node.Addresses = append(node.Addresses, instanceAddresses(running)...)
	return node, nil
}

// networkInterface builds the bootstrap instance's single NIC, with an
// ephemeral external IP when PublicIP is set.
func (p *Provider) networkInterface() *compute.NetworkInterface {
	ni := &compute.NetworkInterface{
		Network:    fmt.Sprintf("global/networks/%s", p.network()),
		Subnetwork: fmt.Sprintf("regions/%s/subnetworks/%s", p.cfg.Region, p.subnet()),
	}
	if p.cfg.PublicIP {
		ni.AccessConfigs = []*compute.AccessConfig{{
			Name: "External NAT",
			Type: "ONE_TO_ONE_NAT",
		}}
	}
	return ni
}

// metadata builds the instance metadata: the cloud-init user-data CAPG reads,
// plus any SSH keys for diagnosing a failed bootstrap.
func (p *Provider) metadata(userData string) *compute.Metadata {
	items := []*compute.MetadataItems{{Key: metadataKeyUserData, Value: ptr(userData)}}
	if len(p.cfg.AuthorizedKeys) > 0 {
		var b strings.Builder
		for _, k := range p.cfg.AuthorizedKeys {
			fmt.Fprintf(&b, "%s:%s\n", p.loginUser(), k)
		}
		items = append(items, &compute.MetadataItems{Key: metadataKeySSHKeys, Value: ptr(b.String())})
	}
	return &compute.Metadata{Items: items}
}

// waitRunning polls until the instance is RUNNING and returns its description.
func (p *Provider) waitRunning(ctx context.Context, name string) (*compute.Instance, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		inst, err := p.compute.GetInstance(ctx, p.cfg.Project, p.cfg.Zone, name)
		if err != nil {
			return nil, errorf("describing instance %q: %w", name, err)
		}
		switch inst.Status {
		case "RUNNING":
			return inst, nil
		case "TERMINATED", "STOPPING", "STOPPED", "SUSPENDED":
			return nil, errorf("instance %q entered state %q before running", name, inst.Status)
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for instance %q to run (last status %q)", name, inst.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

// instanceAddresses returns an instance's external then internal IPv4
// addresses.
func instanceAddresses(inst *compute.Instance) []string {
	var out []string
	for _, ni := range inst.NetworkInterfaces {
		for _, ac := range ni.AccessConfigs {
			if ac.NatIP != "" {
				out = append(out, ac.NatIP)
			}
		}
	}
	for _, ni := range inst.NetworkInterfaces {
		if ni.NetworkIP != "" {
			out = append(out, ni.NetworkIP)
		}
	}
	return out
}
