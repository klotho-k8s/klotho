package aws

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Launch boots the bootstrap node as a single EC2 instance carrying klotho's
// cloud-init as user-data and the tags CAPA adopts it by, waits for it to run,
// associates the Elastic IP if one was allocated, and returns the node.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	ami := spec.Variables[VarAMIID]
	if ami == "" {
		var err error
		if ami, err = p.resolveAMI(ctx); err != nil {
			return nil, err
		}
	}
	subnet := spec.Variables[VarSubnetID]
	if subnet == "" {
		subnet = p.cfg.SubnetID
	}

	in := &ec2.RunInstancesInput{
		ImageId:      ptr(ami),
		InstanceType: ec2types.InstanceType(p.instanceType()),
		MinCount:     ptr(int32(1)),
		MaxCount:     ptr(int32(1)),
		UserData:     ptr(base64.StdEncoding.EncodeToString(userData)),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags:         instanceTags(spec.Name),
		}},
	}
	if p.cfg.PublicIP {
		// A public IP must be requested through a network interface, which is
		// mutually exclusive with the top-level SubnetId and SecurityGroupIds.
		ni := ec2types.InstanceNetworkInterfaceSpecification{
			DeviceIndex:              ptr(int32(0)),
			SubnetId:                 ptr(subnet),
			AssociatePublicIpAddress: ptr(true),
		}
		if len(p.cfg.SecurityGroupIDs) > 0 {
			ni.Groups = p.cfg.SecurityGroupIDs
		}
		in.NetworkInterfaces = []ec2types.InstanceNetworkInterfaceSpecification{ni}
	} else {
		in.SubnetId = ptr(subnet)
		if len(p.cfg.SecurityGroupIDs) > 0 {
			in.SecurityGroupIds = p.cfg.SecurityGroupIDs
		}
	}
	if p.cfg.Spot {
		in.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
			SpotOptions: &ec2types.SpotMarketOptions{
				SpotInstanceType:             ec2types.SpotInstanceTypeOneTime,
				InstanceInterruptionBehavior: ec2types.InstanceInterruptionBehaviorTerminate,
			},
		}
	}
	if p.cfg.SSHKeyName != "" {
		in.KeyName = ptr(p.cfg.SSHKeyName)
	}
	if p.cfg.IAMInstanceProfile != "" {
		in.IamInstanceProfile = &ec2types.IamInstanceProfileSpecification{Name: ptr(p.cfg.IAMInstanceProfile)}
	}

	out, err := p.ec2.RunInstances(ctx, in)
	if err != nil {
		return nil, errorf("launching the bootstrap instance: %w", err)
	}
	if len(out.Instances) == 0 {
		return nil, errorf("RunInstances returned no instance")
	}
	inst := out.Instances[0]
	id := awsString(inst.InstanceId)

	running, err := p.waitRunning(ctx, id)
	if err != nil {
		return nil, err
	}
	az := ""
	if running.Placement != nil {
		az = awsString(running.Placement.AvailabilityZone)
	}

	if err := p.associateEIP(ctx, spec.Name, id); err != nil {
		return nil, err
	}

	node := &cluster.Node{
		ID:   providerID(az, id),
		Name: MachineName(spec.Name),
	}
	if ip := awsString(running.PublicIpAddress); ip != "" {
		node.Addresses = append(node.Addresses, ip)
	}
	if ip := awsString(running.PrivateIpAddress); ip != "" {
		node.Addresses = append(node.Addresses, ip)
	}
	return node, nil
}

// waitRunning polls until the instance is running and returns its description.
func (p *Provider) waitRunning(ctx context.Context, id string) (*ec2types.Instance, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil {
			return nil, errorf("describing instance %s: %w", id, err)
		}
		inst := firstInstance(out)
		if inst != nil && inst.State != nil && inst.State.Name == ec2types.InstanceStateNameRunning {
			return inst, nil
		}
		if inst != nil && inst.State != nil {
			switch inst.State.Name {
			case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown, ec2types.InstanceStateNameStopped:
				return nil, errorf("instance %s entered state %q before running", id, inst.State.Name)
			}
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for instance %s to run", id)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

// associateEIP finds the cluster's allocated Elastic IP (by tag) and associates
// it with the instance. It is a no-op when none was allocated.
func (p *Provider) associateEIP(ctx context.Context, clusterName, instanceID string) error {
	out, err := p.ec2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: clusterFilters(clusterName)})
	if err != nil {
		return errorf("looking up the Elastic IP: %w", err)
	}
	if len(out.Addresses) == 0 {
		return nil
	}
	alloc := out.Addresses[0].AllocationId
	if _, err := p.ec2.AssociateAddress(ctx, &ec2.AssociateAddressInput{AllocationId: alloc, InstanceId: ptr(instanceID)}); err != nil {
		return errorf("associating the Elastic IP with %s: %w", instanceID, err)
	}
	return nil
}

// instanceTags are klotho's ownership tag plus the CAPA adoption tags.
func instanceTags(clusterName string) []ec2types.Tag {
	m := bootstrapTags(clusterName)
	tags := make([]ec2types.Tag, 0, len(m))
	for k, v := range m {
		tags = append(tags, ec2types.Tag{Key: ptr(k), Value: ptr(v)})
	}
	return tags
}

// clusterFilters match resources carrying klotho's ownership tag for a cluster.
func clusterFilters(clusterName string) []ec2types.Filter {
	return []ec2types.Filter{{Name: ptr("tag:" + ClusterTagKey), Values: []string{Tag(clusterName)}}}
}

func firstInstance(out *ec2.DescribeInstancesOutput) *ec2types.Instance {
	for i := range out.Reservations {
		for j := range out.Reservations[i].Instances {
			return &out.Reservations[i].Instances[j]
		}
	}
	return nil
}
