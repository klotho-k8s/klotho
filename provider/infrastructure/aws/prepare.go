package aws

import (
	"context"
	"sort"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// default AMI search: Canonical's Ubuntu 22.04 server cloud images.
const (
	defaultAMIOwner       = "099720109477" // Canonical
	defaultAMINamePattern = "ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"
)

// Prepare validates the region, VPC and subnet, resolves the bootstrap AMI,
// and determines the control plane endpoint, allocating an Elastic IP when
// asked. It returns the endpoint, anything it created (so a failed run can be
// swept), and the template variables the generated bundle needs.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if p.cfg.Region == "" {
		return nil, errorf("region is required")
	}
	if p.cfg.VPCID == "" || p.cfg.SubnetID == "" {
		// Creating a VPC/subnet/IGW/SG is not implemented yet; require an
		// existing network. See the PR notes.
		return nil, errorf("vpcId and subnetId are required (VPC creation is not implemented yet)")
	}
	if err := p.validateNetwork(ctx); err != nil {
		return nil, err
	}

	ami, err := p.resolveAMI(ctx)
	if err != nil {
		return nil, err
	}

	prepared := &infrastructure.Prepared{}
	endpoint, created, err := p.resolveEndpoint(ctx, spec.Name)
	if err != nil {
		return nil, err
	}
	prepared.Endpoint = endpoint
	prepared.Created = created

	prepared.Variables = map[string]string{
		VarRegion:            p.cfg.Region,
		VarControlPlaneType:  p.instanceType(),
		VarNodeType:          p.instanceType(),
		VarAMIID:             ami,
		VarVPCID:             p.cfg.VPCID,
		VarSubnetID:          p.cfg.SubnetID,
		VarControlPlaneCount: "1",
	}
	if p.cfg.SSHKeyName != "" {
		prepared.Variables[VarSSHKeyName] = p.cfg.SSHKeyName
	}
	return prepared, nil
}

// validateNetwork confirms the VPC, subnet and (if given) key pair exist.
func (p *Provider) validateNetwork(ctx context.Context) error {
	v, err := p.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{p.cfg.VPCID}})
	if err != nil {
		return errorf("describing vpc %s: %w", p.cfg.VPCID, err)
	}
	if len(v.Vpcs) == 0 {
		return errorf("vpc %s not found", p.cfg.VPCID)
	}
	s, err := p.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{p.cfg.SubnetID}})
	if err != nil {
		return errorf("describing subnet %s: %w", p.cfg.SubnetID, err)
	}
	if len(s.Subnets) == 0 {
		return errorf("subnet %s not found", p.cfg.SubnetID)
	}
	if p.cfg.SSHKeyName != "" {
		if _, err := p.ec2.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{KeyNames: []string{p.cfg.SSHKeyName}}); err != nil {
			return errorf("key pair %q: %w", p.cfg.SSHKeyName, err)
		}
	}
	return nil
}

// resolveAMI returns the configured AMI id, or the newest image matching the
// owner and name pattern (Ubuntu 22.04 by default).
func (p *Provider) resolveAMI(ctx context.Context) (string, error) {
	if p.cfg.AMIID != "" {
		return p.cfg.AMIID, nil
	}
	owner := p.cfg.AMIOwner
	if owner == "" {
		owner = defaultAMIOwner
	}
	pattern := p.cfg.AMINamePattern
	if pattern == "" {
		pattern = defaultAMINamePattern
	}
	out, err := p.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{owner},
		Filters: []ec2types.Filter{
			{Name: ptr("name"), Values: []string{pattern}},
			{Name: ptr("state"), Values: []string{"available"}},
			{Name: ptr("architecture"), Values: []string{"x86_64"}},
		},
	})
	if err != nil {
		return "", errorf("searching for an AMI (%s %q): %w", owner, pattern, err)
	}
	if len(out.Images) == 0 {
		return "", errorf("no AMI matched owner %s pattern %q; set aws.amiId", owner, pattern)
	}
	imgs := out.Images
	sort.Slice(imgs, func(i, j int) bool {
		return awsString(imgs[i].CreationDate) > awsString(imgs[j].CreationDate)
	})
	return awsString(imgs[0].ImageId), nil
}

// resolveEndpoint returns the control plane endpoint. A configured endpoint is
// validated only; otherwise an Elastic IP is allocated (and recorded) when
// AllocateElasticIP is set.
func (p *Provider) resolveEndpoint(ctx context.Context, clusterName string) (cluster.Endpoint, []cluster.Resource, error) {
	if p.cfg.ControlPlaneEndpoint != "" {
		return cluster.Endpoint{Host: p.cfg.ControlPlaneEndpoint, Port: p.port()}, nil, nil
	}
	if !p.cfg.AllocateElasticIP {
		return cluster.Endpoint{}, nil, errorf("controlPlaneEndpoint is required, or set aws.allocateElasticIp to allocate one")
	}
	out, err := p.ec2.AllocateAddress(ctx, &ec2.AllocateAddressInput{
		Domain: ec2types.DomainTypeVpc,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeElasticIp,
			Tags:         tagsFor(clusterName),
		}},
	})
	if err != nil {
		return cluster.Endpoint{}, nil, errorf("allocating an Elastic IP: %w", err)
	}
	created := []cluster.Resource{{Kind: "eip", ID: awsString(out.AllocationId), Name: awsString(out.PublicIp)}}
	return cluster.Endpoint{Host: awsString(out.PublicIp), Port: p.port()}, created, nil
}

// tagsFor builds the EC2 tag list carrying klotho's ownership tag for a
// cluster.
func tagsFor(clusterName string) []ec2types.Tag {
	return []ec2types.Tag{{Key: ptr(ClusterTagKey), Value: ptr(Tag(clusterName))}}
}
