package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports the VPCs, subnets and key pairs a cluster template can be
// prefilled with. It is read-only.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	inv := infrastructure.Inventory{}

	vpcs, err := p.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		return nil, errorf("listing vpcs: %w", err)
	}
	for _, v := range vpcs.Vpcs {
		inv["vpc"] = append(inv["vpc"], infrastructure.Item{
			ID:    awsString(v.VpcId),
			Name:  nameTag(v.Tags),
			Attrs: map[string]string{"cidr": awsString(v.CidrBlock)},
		})
	}

	subnets, err := p.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	if err != nil {
		return nil, errorf("listing subnets: %w", err)
	}
	for _, s := range subnets.Subnets {
		inv["subnet"] = append(inv["subnet"], infrastructure.Item{
			ID:   awsString(s.SubnetId),
			Name: nameTag(s.Tags),
			Attrs: map[string]string{
				"vpc":  awsString(s.VpcId),
				"az":   awsString(s.AvailabilityZone),
				"cidr": awsString(s.CidrBlock),
			},
		})
	}

	keys, err := p.ec2.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{})
	if err != nil {
		return nil, errorf("listing key pairs: %w", err)
	}
	for _, k := range keys.KeyPairs {
		inv["keypair"] = append(inv["keypair"], infrastructure.Item{ID: awsString(k.KeyPairId), Name: awsString(k.KeyName)})
	}

	return inv, nil
}
