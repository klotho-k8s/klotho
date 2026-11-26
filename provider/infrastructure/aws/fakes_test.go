package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeEC2 is an in-memory EC2API for tests. It records the calls that matter
// and serves instances and addresses so Launch/Plan/Teardown can be exercised
// without AWS.
type fakeEC2 struct {
	images   []ec2types.Image
	vpcs     []ec2types.Vpc
	subnets  []ec2types.Subnet
	keyPairs []ec2types.KeyPairInfo

	instances map[string]*ec2types.Instance
	addresses []ec2types.Address

	runInput    *ec2.RunInstancesInput
	nextID      string
	nextAZ      string
	allocSeq    int
	terminated  []string
	released    []string
	associated  map[string]string
	createTags  int
	allocatedIP string
}

func newFakeEC2() *fakeEC2 {
	return &fakeEC2{instances: map[string]*ec2types.Instance{}, associated: map[string]string{}, nextID: "i-0bootstrap", nextAZ: "us-east-1a"}
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.runInput = in
	var tags []ec2types.Tag
	for _, ts := range in.TagSpecifications {
		if ts.ResourceType == ec2types.ResourceTypeInstance {
			tags = ts.Tags
		}
	}
	inst := &ec2types.Instance{
		InstanceId:       ptr(f.nextID),
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Placement:        &ec2types.Placement{AvailabilityZone: ptr(f.nextAZ)},
		PublicIpAddress:  ptr("203.0.113.10"),
		PrivateIpAddress: ptr("10.0.0.10"),
		Tags:             tags,
	}
	f.instances[f.nextID] = inst
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{*inst}}, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	var matched []ec2types.Instance
	want := map[string]bool{}
	for _, id := range in.InstanceIds {
		want[id] = true
	}
	tagCluster := filterValue(in.Filters, "tag:"+ClusterTagKey)
	for id, inst := range f.instances {
		if len(in.InstanceIds) > 0 && !want[id] {
			continue
		}
		if tagCluster != "" && tagValue(inst.Tags, ClusterTagKey) != tagCluster {
			continue
		}
		matched = append(matched, *inst)
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: matched}}}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	for _, id := range in.InstanceIds {
		f.terminated = append(f.terminated, id)
		if inst, ok := f.instances[id]; ok {
			inst.State = &ec2types.InstanceState{Name: ec2types.InstanceStateNameTerminated}
		}
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) CreateTags(_ context.Context, _ *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.createTags++
	return &ec2.CreateTagsOutput{}, nil
}

func (f *fakeEC2) DescribeImages(_ context.Context, _ *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: f.images}, nil
}

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	if len(in.SubnetIds) > 0 && len(f.subnets) == 0 {
		return &ec2.DescribeSubnetsOutput{}, nil
	}
	return &ec2.DescribeSubnetsOutput{Subnets: f.subnets}, nil
}

func (f *fakeEC2) DescribeSecurityGroups(_ context.Context, _ *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{}, nil
}

func (f *fakeEC2) DescribeKeyPairs(_ context.Context, _ *ec2.DescribeKeyPairsInput, _ ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error) {
	return &ec2.DescribeKeyPairsOutput{KeyPairs: f.keyPairs}, nil
}

func (f *fakeEC2) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	if len(in.VpcIds) > 0 && len(f.vpcs) == 0 {
		return &ec2.DescribeVpcsOutput{}, nil
	}
	return &ec2.DescribeVpcsOutput{Vpcs: f.vpcs}, nil
}

func (f *fakeEC2) AllocateAddress(_ context.Context, in *ec2.AllocateAddressInput, _ ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error) {
	f.allocSeq++
	alloc := fmt.Sprintf("eipalloc-%03d", f.allocSeq)
	ip := "198.51.100.5"
	f.allocatedIP = ip
	var tags []ec2types.Tag
	for _, ts := range in.TagSpecifications {
		if ts.ResourceType == ec2types.ResourceTypeElasticIp {
			tags = ts.Tags
		}
	}
	f.addresses = append(f.addresses, ec2types.Address{AllocationId: ptr(alloc), PublicIp: ptr(ip), Tags: tags})
	return &ec2.AllocateAddressOutput{AllocationId: ptr(alloc), PublicIp: ptr(ip)}, nil
}

func (f *fakeEC2) ReleaseAddress(_ context.Context, in *ec2.ReleaseAddressInput, _ ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	f.released = append(f.released, awsString(in.AllocationId))
	return &ec2.ReleaseAddressOutput{}, nil
}

func (f *fakeEC2) AssociateAddress(_ context.Context, in *ec2.AssociateAddressInput, _ ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error) {
	f.associated[awsString(in.AllocationId)] = awsString(in.InstanceId)
	return &ec2.AssociateAddressOutput{}, nil
}

func (f *fakeEC2) DescribeAddresses(_ context.Context, in *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	tagCluster := filterValue(in.Filters, "tag:"+ClusterTagKey)
	var matched []ec2types.Address
	for _, a := range f.addresses {
		if tagCluster == "" || tagValue(a.Tags, ClusterTagKey) == tagCluster {
			matched = append(matched, a)
		}
	}
	return &ec2.DescribeAddressesOutput{Addresses: matched}, nil
}

func filterValue(filters []ec2types.Filter, name string) string {
	for _, fl := range filters {
		if awsString(fl.Name) == name && len(fl.Values) > 0 {
			return fl.Values[0]
		}
	}
	return ""
}

func tagValue(tags []ec2types.Tag, key string) string {
	for _, t := range tags {
		if awsString(t.Key) == key {
			return awsString(t.Value)
		}
	}
	return ""
}

func hasTag(tags []ec2types.Tag, key, value string) bool {
	return tagValue(tags, key) == value
}

func tagKeys(tags []ec2types.Tag) string {
	var ks []string
	for _, t := range tags {
		ks = append(ks, awsString(t.Key))
	}
	return strings.Join(ks, ",")
}
