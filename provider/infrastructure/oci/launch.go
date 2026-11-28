package oci

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Instance metadata keys CAPOCI reads. user_data carries the base64 cloud-init
// (cloud/scope/machine.go sets metadata["user_data"] the same way); klotho boots
// node zero with the identical key so the adopted instance used the same path.
const (
	metadataKeyUserData = "user_data"
	metadataKeySSHKeys  = "ssh_authorized_keys"
)

// Launch boots the bootstrap node as a single OCI compute instance whose display
// name is MachineName(cluster) — the name CAPOCI's display-name lookup adopts by
// — carrying klotho's cloud-init as the "user_data" metadata item and the
// freeform tags (CAPOCI's two ownership tags plus klotho's) that make CAPOCI
// recognise and adopt it. It waits for the instance to run and returns the node.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	name := MachineName(spec.Name)
	shape := spec.Variables[VarControlPlaneShape]
	if shape == "" {
		shape = p.shape()
	}
	image := spec.Variables[VarImageID]
	if image == "" {
		image = p.cfg.ImageID
	}

	details := core.LaunchInstanceDetails{
		AvailabilityDomain: common.String(p.cfg.AvailabilityDomain),
		CompartmentId:      common.String(p.cfg.CompartmentID),
		DisplayName:        common.String(name),
		Shape:              common.String(shape),
		SourceDetails: core.InstanceSourceViaImageDetails{
			ImageId:             common.String(image),
			BootVolumeSizeInGBs: common.Int64(p.bootVolumeGBs()),
		},
		CreateVnicDetails: &core.CreateVnicDetails{
			SubnetId:       common.String(p.cfg.SubnetID),
			AssignPublicIp: common.Bool(p.cfg.AssignPublicIP),
			NsgIds:         p.cfg.NSGIDs,
			DisplayName:    common.String(name),
			FreeformTags:   p.clusterFreeformTags(spec.Name),
		},
		Metadata:     p.metadata(userData),
		FreeformTags: p.clusterFreeformTags(spec.Name),
	}
	// A flexible shape needs its OCPU/memory configuration; a fixed shape must
	// not carry one. Flex shapes are named "...Flex".
	if isFlexShape(shape) {
		details.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{
			Ocpus:       common.Float32(p.ocpus()),
			MemoryInGBs: common.Float32(p.memoryGBs()),
		}
	}

	inst, err := p.client.LaunchInstance(ctx, details)
	if err != nil {
		return nil, errorf("launching the bootstrap instance: %w", err)
	}

	running, err := p.waitRunning(ctx, strVal(inst.Id))
	if err != nil {
		return nil, err
	}

	node := &cluster.Node{
		ID:   providerID(strVal(running.Id)),
		Name: name,
	}
	// Addresses are best-effort diagnostics; a VNIC that is not resolvable yet
	// must not fail the launch.
	if addrs, err := p.client.PrimaryVnicAddresses(ctx, p.cfg.CompartmentID, strVal(running.Id)); err == nil {
		node.Addresses = addrs
	}
	return node, nil
}

// metadata builds the instance metadata: the cloud-init user_data CAPOCI reads,
// plus any SSH keys for diagnosing a failed bootstrap.
func (p *Provider) metadata(userData []byte) map[string]string {
	m := map[string]string{
		metadataKeyUserData: base64.StdEncoding.EncodeToString(userData),
	}
	if len(p.cfg.SSHAuthorizedKeys) > 0 {
		m[metadataKeySSHKeys] = strings.Join(p.cfg.SSHAuthorizedKeys, "\n")
	}
	return m
}

// waitRunning polls until the instance is RUNNING and returns its description.
func (p *Provider) waitRunning(ctx context.Context, id string) (*core.Instance, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		inst, err := p.client.GetInstance(ctx, id)
		if err != nil {
			return nil, errorf("describing instance %q: %w", id, err)
		}
		switch inst.LifecycleState {
		case core.InstanceLifecycleStateRunning:
			return &inst, nil
		case core.InstanceLifecycleStateTerminating, core.InstanceLifecycleStateTerminated,
			core.InstanceLifecycleStateStopping, core.InstanceLifecycleStateStopped:
			return nil, errorf("instance %q entered state %q before running", id, inst.LifecycleState)
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for instance %q to run (last state %q)", id, inst.LifecycleState)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

// isFlexShape reports whether shape is a flexible shape, which requires an
// OCPU/memory shape configuration (fixed shapes reject one).
func isFlexShape(shape string) bool {
	return strings.HasSuffix(shape, ".Flex") || strings.Contains(shape, ".Flex.")
}
