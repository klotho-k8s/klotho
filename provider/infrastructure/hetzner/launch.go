package hetzner

import (
	"context"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Launch boots the bootstrap node as a single Hetzner Cloud server named
// MachineName(cluster) — the name CAPH gives the server it would create — carrying
// klotho's cloud-init as the server's user_data and klotho's ownership label, so
// CAPH's name-based adoption takes it over. It waits for the server to run and
// returns the node, its provider ID and its addresses.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	name := MachineName(spec.Name)

	location := spec.Variables[VarLocation]
	if location == "" {
		location = p.cfg.Location
	}
	serverType := spec.Variables[VarControlPlaneType]
	if serverType == "" {
		serverType = p.serverType()
	}
	image := spec.Variables[VarImage]
	if image == "" {
		image = p.image()
	}

	sshKeys, err := p.resolveSSHKeys(ctx)
	if err != nil {
		return nil, err
	}

	opts := hcloud.ServerCreateOpts{
		Name:             name,
		ServerType:       &hcloud.ServerType{Name: serverType},
		Image:            &hcloud.Image{Name: image},
		Location:         &hcloud.Location{Name: location},
		UserData:         string(userData),
		Labels:           p.serverLabels(spec.Name),
		SSHKeys:          sshKeys,
		StartAfterCreate: hcloud.Ptr(true),
	}

	res, err := p.client.CreateServer(ctx, opts)
	if err != nil {
		return nil, errorf("creating the bootstrap server: %w", err)
	}
	if res.Server == nil {
		return nil, errorf("creating the bootstrap server: hcloud returned no server")
	}

	running, err := p.waitRunning(ctx, res.Server.ID)
	if err != nil {
		return nil, err
	}

	node := &cluster.Node{
		ID:        providerID(running.ID),
		Name:      name,
		Addresses: serverAddresses(running),
	}
	return node, nil
}

// resolveSSHKeys looks up the configured SSH key names and returns their hcloud
// references for the create request.
func (p *Provider) resolveSSHKeys(ctx context.Context) ([]*hcloud.SSHKey, error) {
	keys := make([]*hcloud.SSHKey, 0, len(p.cfg.SSHKeys))
	for _, name := range p.cfg.SSHKeys {
		key, err := p.client.GetSSHKey(ctx, name)
		if err != nil {
			return nil, errorf("looking up SSH key %q: %w", name, err)
		}
		if key == nil {
			return nil, errorf("SSH key %q not found in the Hetzner project", name)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// waitRunning polls until the server is running and returns its description.
func (p *Provider) waitRunning(ctx context.Context, id int64) (*hcloud.Server, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		srv, err := p.client.GetServer(ctx, id)
		if err != nil {
			return nil, errorf("describing server %d: %w", id, err)
		}
		if srv == nil {
			return nil, errorf("server %d disappeared before it ran", id)
		}
		switch srv.Status {
		case hcloud.ServerStatusRunning:
			return srv, nil
		case hcloud.ServerStatusOff, hcloud.ServerStatusDeleting:
			return nil, errorf("server %d entered state %q before running", id, srv.Status)
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for server %d to run (last status %q)", id, srv.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

// serverAddresses returns the server's public then private IPv4/IPv6 addresses.
func serverAddresses(srv *hcloud.Server) []string {
	var out []string
	if ip := srv.PublicNet.IPv4.IP; ip != nil {
		out = append(out, ip.String())
	}
	if ip := srv.PublicNet.IPv6.IP; ip != nil {
		out = append(out, ip.String())
	}
	for _, pn := range srv.PrivateNet {
		if pn.IP != nil {
			out = append(out, pn.IP.String())
		}
	}
	return out
}
