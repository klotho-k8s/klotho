package openstack

import (
	"context"
	"time"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Launch boots the bootstrap node as a single Nova server named
// MachineName(cluster) — the name CAPO derives the server name from — carrying
// klotho's cloud-init as the server user-data and klotho's ownership tag, so
// CAPO's name-keyed GetInstanceStatusByName adopts it. It waits for the server
// to become ACTIVE, associates the control plane floating IP Prepare allocated
// (if any) so the endpoint is reachable during converge, and returns the node.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	name := MachineName(spec.Name)

	flavor := varOr(spec, VarControlPlaneFlavor, p.cfg.Flavor)
	image := varOr(spec, VarImageName, p.cfg.Image)
	networkID := spec.Variables[VarNetworkID]

	flavorID, err := p.client.FlavorIDByName(ctx, flavor)
	if err != nil {
		return nil, errorf("resolving flavor %q: %w", flavor, err)
	}
	imageID, err := p.client.ImageIDByName(ctx, image)
	if err != nil {
		return nil, errorf("resolving image %q: %w", image, err)
	}
	if networkID == "" {
		if networkID, err = p.client.NetworkIDByName(ctx, p.cfg.Network); err != nil {
			return nil, errorf("resolving network %q: %w", p.cfg.Network, err)
		}
	}

	srv, err := p.client.CreateServer(ctx, CreateServerOpts{
		Name:             name,
		FlavorID:         flavorID,
		ImageID:          imageID,
		NetworkID:        networkID,
		SSHKeyName:       p.cfg.SSHKeyName,
		SecurityGroups:   p.cfg.SecurityGroups,
		AvailabilityZone: p.cfg.AvailabilityZone,
		UserData:         userData,
		Tags:             p.serverTags(spec.Name),
	})
	if err != nil {
		return nil, errorf("launching the bootstrap server: %w", err)
	}

	running, err := p.waitActive(ctx, srv.ID)
	if err != nil {
		return nil, err
	}

	// Associate the floating IP klotho allocated so node zero answers at the
	// control plane endpoint before CAPO is running. A user-supplied endpoint is
	// the user's to point at node zero (klotho allocated nothing to associate).
	if p.fipID != "" {
		if err := p.client.AssociateFloatingIP(ctx, p.fipID, srv.ID); err != nil {
			return nil, errorf("associating the control plane floating IP with %q: %w", name, err)
		}
	}

	node := &cluster.Node{
		// Empty region, matching the template's kubelet provider-id
		// ("openstack:///<uuid>") and the value CAPO sets when identityRef.region
		// is unset. Region selects the service endpoint, not the provider ID.
		ID:        providerID("", srv.ID),
		Name:      name,
		Addresses: append([]string{}, running.Addresses...),
	}
	if p.cfg.ControlPlaneEndpoint != "" {
		node.Addresses = append(node.Addresses, p.cfg.ControlPlaneEndpoint)
	}
	return node, nil
}

// waitActive polls until the server is ACTIVE and returns its description.
func (p *Provider) waitActive(ctx context.Context, id string) (*Server, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		srv, err := p.client.GetServer(ctx, id)
		if err != nil {
			return nil, errorf("describing server %q: %w", id, err)
		}
		switch srv.Status {
		case "ACTIVE":
			return srv, nil
		case "ERROR", "DELETED":
			return nil, errorf("server %q entered state %q before becoming active", id, srv.Status)
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for server %q to become active (last status %q)", id, srv.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

// varOr returns spec.Variables[key] when set, else fallback.
func varOr(spec *cluster.Spec, key, fallback string) string {
	if spec != nil {
		if v := spec.Variables[key]; v != "" {
			return v
		}
	}
	return fallback
}
