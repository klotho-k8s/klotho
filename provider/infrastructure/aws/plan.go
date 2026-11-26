package aws

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Resource kinds in a Plan.
const (
	ResourceKindInstance = "instance"
	ResourceKindEIP      = "eip"
)

// Plan lists the cluster's AWS resources by klotho's ownership tag: every
// non-terminated EC2 instance (including machines CAPA created from the tagged
// templates) and any Elastic IP klotho allocated, plus anything recorded in
// state that is not found by tag.
func (p *Provider) Plan(ctx context.Context, state *cluster.State) (*infrastructure.Plan, error) {
	if state == nil || state.Spec.Name == "" {
		return nil, errorf("state has no cluster name")
	}
	name := state.Spec.Name
	plan := &infrastructure.Plan{}
	seen := map[string]bool{}
	add := func(r cluster.Resource) {
		key := r.Kind + "|" + r.ID
		if r.ID != "" && !seen[key] {
			seen[key] = true
			plan.Resources = append(plan.Resources, r)
		}
	}

	inst, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: append(clusterFilters(name), ec2types.Filter{
			Name:   ptr("instance-state-name"),
			Values: []string{"pending", "running", "stopping", "stopped"},
		}),
	})
	if err != nil {
		return nil, errorf("listing instances: %w", err)
	}
	for i := range inst.Reservations {
		for j := range inst.Reservations[i].Instances {
			in := inst.Reservations[i].Instances[j]
			add(cluster.Resource{Kind: ResourceKindInstance, ID: awsString(in.InstanceId), Name: nameTag(in.Tags)})
		}
	}

	eips, err := p.ec2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: clusterFilters(name)})
	if err != nil {
		return nil, errorf("listing Elastic IPs: %w", err)
	}
	for _, a := range eips.Addresses {
		add(cluster.Resource{Kind: ResourceKindEIP, ID: awsString(a.AllocationId), Name: awsString(a.PublicIp)})
	}

	// Anything state recorded that the tag search did not surface.
	for _, r := range state.Resources {
		add(r)
	}
	return plan, nil
}

// Teardown removes everything in the plan: instances are terminated (and waited
// out), then Elastic IPs are released. Any VPC/subnet/security group klotho
// created is not yet handled (klotho does not create them yet; see Prepare).
func (p *Provider) Teardown(ctx context.Context, state *cluster.State, plan *infrastructure.Plan) error {
	if plan == nil {
		return nil
	}
	var instanceIDs []string
	var eipAllocs []string
	for _, r := range plan.Resources {
		switch r.Kind {
		case ResourceKindInstance:
			instanceIDs = append(instanceIDs, r.ID)
		case ResourceKindEIP:
			eipAllocs = append(eipAllocs, r.ID)
		}
	}

	if len(instanceIDs) > 0 {
		if _, err := p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: instanceIDs}); err != nil {
			return errorf("terminating instances: %w", err)
		}
		if err := p.waitTerminated(ctx, instanceIDs); err != nil {
			return err
		}
	}
	for _, alloc := range eipAllocs {
		if _, err := p.ec2.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: ptr(alloc)}); err != nil {
			return errorf("releasing Elastic IP %s: %w", alloc, err)
		}
	}
	return nil
}

// waitTerminated waits, best effort, for the instances to leave the running
// state so a released Elastic IP is not still associated.
func (p *Provider) waitTerminated(ctx context.Context, ids []string) error {
	deadline := time.Now().Add(p.launchWait)
	for {
		out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
		if err != nil {
			// The instances may already be gone; treat a describe error as done.
			return nil
		}
		allGone := true
		for i := range out.Reservations {
			for j := range out.Reservations[i].Instances {
				st := out.Reservations[i].Instances[j].State
				if st != nil && st.Name != ec2types.InstanceStateNameTerminated {
					allGone = false
				}
			}
		}
		if allGone {
			return nil
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

func nameTag(tags []ec2types.Tag) string {
	for _, t := range tags {
		if awsString(t.Key) == nameTagKey {
			return awsString(t.Value)
		}
	}
	return ""
}
