package linode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/linode/linodego/v2"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Launch boots the bootstrap node as a single Linode instance carrying klotho's
// cloud-init as Metadata user-data and klotho's ownership tag, labelled
// MachineName(cluster) so CAPL adopts it. It waits for the instance to run and
// returns the node, its provider ID and its addresses.
func (p *Provider) Launch(ctx context.Context, spec *cluster.Spec, userData []byte) (*cluster.Node, error) {
	region := spec.Variables[VarRegion]
	if region == "" {
		region = p.cfg.Region
	}
	typ := spec.Variables[VarControlPlaneType]
	if typ == "" {
		typ = p.instanceType()
	}
	image := spec.Variables[VarImage]
	if image == "" {
		image = p.image()
	}

	rootPass := p.cfg.RootPass
	if rootPass == "" {
		var err error
		if rootPass, err = randomRootPass(); err != nil {
			return nil, err
		}
	}

	opts := linodego.InstanceCreateOptions{
		Region:   region,
		Type:     typ,
		Label:    MachineName(spec.Name),
		Image:    image,
		RootPass: rootPass,
		Tags:     p.instanceTags(spec.Name),
		Metadata: &linodego.InstanceMetadataOptions{
			UserData: base64.StdEncoding.EncodeToString(userData),
		},
	}
	if len(p.cfg.AuthorizedKeys) > 0 {
		opts.AuthorizedKeys = p.cfg.AuthorizedKeys
	}
	if p.cfg.FirewallID > 0 {
		opts.FirewallID = p.cfg.FirewallID
	}

	inst, err := p.client.CreateInstance(ctx, opts)
	if err != nil {
		return nil, errorf("creating the bootstrap instance: %w", err)
	}

	running, err := p.waitRunning(ctx, inst.ID)
	if err != nil {
		return nil, err
	}

	node := &cluster.Node{
		ID:   providerID(running.ID),
		Name: MachineName(spec.Name),
	}
	for _, ip := range running.IPv4 {
		node.Addresses = append(node.Addresses, ip.String())
	}
	return node, nil
}

// waitRunning polls until the instance is running and returns its description.
func (p *Provider) waitRunning(ctx context.Context, id int) (*linodego.Instance, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		inst, err := p.client.GetInstance(ctx, id)
		if err != nil {
			return nil, errorf("describing instance %d: %w", id, err)
		}
		switch inst.Status {
		case linodego.InstanceRunning:
			return inst, nil
		case linodego.InstanceOffline:
			// An instance created with an image boots on its own; a lingering
			// offline state past the first poll means it will not come up.
		}
		if !time.Now().Before(deadline) {
			return nil, errorf("timed out waiting for instance %d to run (last status %q)", id, inst.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

// randomRootPass returns a strong random password for the instance's root
// account. klotho reaches the node through cloud-init, so the password is never
// used; Linode only requires one be set when an image is deployed.
func randomRootPass() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", errorf("generating a root password: %w", err)
	}
	// Mix in character classes Linode's strength check expects.
	return "Aa1!" + base64.RawURLEncoding.EncodeToString(b), nil
}
