package gcp

import (
	"context"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ResourceKindAddress is the Plan/Created kind for a reserved global IP.
const ResourceKindAddress = "address"

// Prepare validates the project, zone, network and subnet, resolves the
// bootstrap image, and determines the control plane endpoint. Because stock
// CAPG always fronts the API server with a managed external load balancer and
// overwrites spec.controlPlaneEndpoint with that load balancer's address,
// klotho reserves the SAME global static IP CAPG's load balancer uses
// (named addressName(cluster) = "<cluster>-apiserver") here, so the endpoint is
// known before the certificates and node zero's cloud-init are minted. CAPG's
// createOrGetAddress is get-or-create by that name, so it adopts the reserved
// address rather than allocating its own. A user-supplied ControlPlaneEndpoint
// is validated and used as-is (no address is reserved).
//
// It returns the endpoint, anything it created (so a failed run can be swept),
// and the template variables the generated bundle needs.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if p.cfg.Project == "" {
		return nil, errorf("project is required (set gcp.project)")
	}
	if p.cfg.Region == "" {
		return nil, errorf("region is required (set gcp.region)")
	}
	if p.cfg.Zone == "" {
		return nil, errorf("zone is required (set gcp.zone)")
	}
	if err := p.validateImage(ctx); err != nil {
		return nil, err
	}
	if err := p.validateNetwork(ctx); err != nil {
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
		VarProject:           p.cfg.Project,
		VarRegion:            p.cfg.Region,
		VarZone:              p.cfg.Zone,
		VarNetwork:           p.network(),
		VarSubnet:            p.subnet(),
		VarControlPlaneType:  p.machineType(),
		VarNodeType:          p.machineType(),
		VarImage:             p.imageURL(),
		VarControlPlaneCount: "1",
	}
	return prepared, nil
}

// validateImage confirms the configured image or image family resolves.
func (p *Provider) validateImage(ctx context.Context) error {
	if p.cfg.Image != "" && !imageHasProject(p.cfg.Image) {
		if _, err := p.compute.GetImage(ctx, p.imageProject(), p.cfg.Image); err != nil {
			return errorf("image %q in project %q: %w", p.cfg.Image, p.imageProject(), err)
		}
		return nil
	}
	if p.cfg.Image != "" {
		// A fully-qualified image path cannot be validated without parsing the
		// project out of it; trust it.
		return nil
	}
	if _, err := p.compute.GetImageFromFamily(ctx, p.imageProject(), p.imageFamily()); err != nil {
		return errorf("image family %q in project %q: %w", p.imageFamily(), p.imageProject(), err)
	}
	return nil
}

// validateNetwork confirms the VPC network and the region subnet exist.
func (p *Provider) validateNetwork(ctx context.Context) error {
	if _, err := p.compute.GetNetwork(ctx, p.cfg.Project, p.network()); err != nil {
		return errorf("network %q: %w", p.network(), err)
	}
	if _, err := p.compute.GetSubnetwork(ctx, p.cfg.Project, p.cfg.Region, p.subnet()); err != nil {
		return errorf("subnetwork %q in region %q: %w", p.subnet(), p.cfg.Region, err)
	}
	return nil
}

// resolveEndpoint returns the control plane endpoint. A configured endpoint is
// used as-is; otherwise the global IP CAPG's load balancer uses is reserved (and
// recorded) so the endpoint is known before boot.
func (p *Provider) resolveEndpoint(ctx context.Context, clusterName string) (cluster.Endpoint, []cluster.Resource, error) {
	if p.cfg.ControlPlaneEndpoint != "" {
		return cluster.Endpoint{Host: p.cfg.ControlPlaneEndpoint, Port: p.port()}, nil, nil
	}
	name := addressName(clusterName)

	// Reserve (or reuse) the global external address CAPG's LB will adopt.
	if existing, err := p.compute.GetGlobalAddress(ctx, p.cfg.Project, name); err == nil {
		if existing.Address == "" {
			return cluster.Endpoint{}, nil, errorf("reserved address %q has no IP yet; retry", name)
		}
		return cluster.Endpoint{Host: existing.Address, Port: p.port()}, nil, nil
	} else if !notFound(err) {
		return cluster.Endpoint{}, nil, errorf("looking up global address %q: %w", name, err)
	}

	op, err := p.compute.InsertGlobalAddress(ctx, p.cfg.Project, &compute.Address{
		Name:        name,
		AddressType: "EXTERNAL",
		IpVersion:   "IPV4",
		Labels:      clusterLabels(clusterName),
		Description: "klotho control plane endpoint for " + clusterName,
	})
	if err != nil {
		return cluster.Endpoint{}, nil, errorf("reserving global address %q: %w", name, err)
	}
	if _, err := p.compute.WaitGlobalOperation(ctx, p.cfg.Project, op.Name); err != nil {
		return cluster.Endpoint{}, nil, errorf("waiting for address %q to be reserved: %w", name, err)
	}

	addr, err := p.waitAddressAssigned(ctx, name)
	if err != nil {
		return cluster.Endpoint{}, nil, err
	}
	created := []cluster.Resource{{Kind: ResourceKindAddress, ID: name, Name: addr}}
	return cluster.Endpoint{Host: addr, Port: p.port()}, created, nil
}

// waitAddressAssigned polls until the reserved global address has an IP.
func (p *Provider) waitAddressAssigned(ctx context.Context, name string) (string, error) {
	deadline := time.Now().Add(p.launchWait)
	for {
		a, err := p.compute.GetGlobalAddress(ctx, p.cfg.Project, name)
		if err != nil {
			return "", errorf("reading reserved address %q: %w", name, err)
		}
		if a.Address != "" {
			return a.Address, nil
		}
		if !time.Now().Before(deadline) {
			return "", errorf("timed out waiting for address %q to be assigned an IP", name)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(p.launchPoll):
		}
	}
}

// imageHasProject reports whether image is a fully-qualified image reference
// (carrying a project path) rather than a bare image name.
func imageHasProject(image string) bool {
	return strings.Contains(image, "/")
}
