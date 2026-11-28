package digitalocean

import (
	"context"
	"time"

	"github.com/digitalocean/godo"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Launch boots the bootstrap node as a single Droplet named MachineName(cluster)
// carrying klotho's cloud-init as the Droplet user-data and klotho's ownership
// tag plus CAPDO's cluster and apiserver role tags, so the control plane load
// balancer forwards to it and CAPDO adopts it (by the provider ID Adopt sets)
// and finds it by tag. It waits for the droplet to run and returns the node, its
// provider ID and its addresses.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	region := spec.Variables[VarRegion]
	if region == "" {
		region = p.cfg.Region
	}
	size := spec.Variables[VarControlPlaneSize]
	if size == "" {
		size = p.size()
	}

	req := &godo.DropletCreateRequest{
		Name:              MachineName(spec.Name),
		Region:            region,
		Size:              size,
		Image:             p.dropletImage(),
		SSHKeys:           p.sshKeys(),
		UserData:          string(userData),
		Tags:              p.bootstrapTags(spec.Name),
		VPCUUID:           p.cfg.VPCUUID,
		PrivateNetworking: true,
		Monitoring:        true,
	}

	droplet, err := p.client.CreateDroplet(ctx, req)
	if err != nil {
		return nil, errorf("creating the bootstrap droplet: %w", err)
	}

	running, err := p.waitRunning(ctx, droplet.ID)
	if err != nil {
		return nil, err
	}

	node := &cluster.Node{
		ID:   providerID(running.ID),
		Name: MachineName(spec.Name),
	}
	if ip, err := running.PublicIPv4(); err == nil && ip != "" {
		node.Addresses = append(node.Addresses, ip)
	}
	if ip, err := running.PrivateIPv4(); err == nil && ip != "" {
		node.Addresses = append(node.Addresses, ip)
	}
	return node, nil
}

// waitRunning polls until the droplet is active and has a public address, and
// returns its description.
func (p *Provider) waitRunning(ctx context.Context, id int) (*godo.Droplet, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		droplet, err := p.client.GetDroplet(ctx, id)
		if err != nil {
			return nil, errorf("describing droplet %d: %w", id, err)
		}
		if droplet.Status == "active" {
			// A freshly active droplet may not have its network populated for a
			// poll or two; wait for a public address before returning.
			if ip, err := droplet.PublicIPv4(); err == nil && ip != "" {
				return droplet, nil
			}
		}
		if droplet.Status == "archive" {
			return nil, errorf("droplet %d entered state %q before running", id, droplet.Status)
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for droplet %d to run (last status %q)", id, droplet.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}
