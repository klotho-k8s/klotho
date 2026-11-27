package libvirt

import (
	"context"
	"slices"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Prepare validates the configuration against the host and resolves the control
// plane endpoint. libvirt has no load balancer, so the endpoint must be a
// static address the user supplies (libvirt.endpointHost or the
// CONTROL_PLANE_ENDPOINT_HOST variable): the address node zero will have on the
// NAT network. Prepare only validates it, it never allocates, so Created is
// always empty. It injects the connection URI, host, endpoint, image, pool and
// network as template variables so a generated bundle resolves to the same
// values this provider boots with.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	r := p.resolve(spec.Variables)
	if r.Image == "" {
		return nil, errorf("no base image: set libvirt.image or the %s variable", VarImage)
	}
	if r.EndpointHost == "" {
		return nil, errorf("no control plane endpoint: libvirt has no load balancer, so set libvirt.endpointHost or the %s variable to the static address node zero will have", VarEndpointHost)
	}
	if r.NetworkType != "network" && r.NetworkType != "bridge" {
		return nil, errorf("network type %q is not one of network, bridge", r.NetworkType)
	}
	// The in-cluster controller reaches libvirtd over the connection URI klotho
	// injects (LIBVIRT_URI -> LibvirtCluster.spec.connectionURI). A local-socket
	// URI is unreachable from a pod, so the controller needs a network URI even
	// when klotho the CLI dials a local socket. Block with a clear finding
	// rather than hand the controller an unreachable qemu:///system.
	if localSocketURI(r.ControllerURI) {
		return nil, errorf("the in-cluster libvirt controller cannot reach the local-socket URI %q; set libvirt.controllerURI (or libvirt.uri) to a qemu+tcp:// or qemu+tls:// URL the controller pods can reach (klotho the CLI may still dial the local socket via libvirt.uri)", r.ControllerURI)
	}

	pools, err := p.client.ListStoragePools(ctx)
	if err != nil {
		return nil, errorf("listing storage pools: %w", err)
	}
	if !slices.Contains(pools, r.StoragePool) {
		return nil, errorf("storage pool %q does not exist on this host (have: %v)", r.StoragePool, pools)
	}

	exists, err := p.client.VolumeExists(ctx, r.StoragePool, r.Image)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errorf("base image volume %q does not exist in storage pool %q; create it from a cloud image first", r.Image, r.StoragePool)
	}

	// Only a libvirt virtual network is a host object we can enumerate; a host
	// bridge is an OS interface the provider does not manage, so it is not
	// validated here.
	if r.NetworkType == "network" {
		nets, err := p.client.ListNetworks(ctx)
		if err != nil {
			return nil, errorf("listing networks: %w", err)
		}
		if !slices.Contains(nets, r.Network) {
			return nil, errorf("libvirt network %q does not exist on this host (have: %v)", r.Network, nets)
		}
	}

	return &infrastructure.Prepared{
		Endpoint: cluster.Endpoint{Host: r.EndpointHost, Port: DefaultPort},
		Variables: map[string]string{
			VarURI:          r.ControllerURI,
			VarLibvirtHost:  r.Host,
			VarEndpointHost: r.EndpointHost,
			VarImage:        r.Image,
			VarPool:         r.StoragePool,
			VarNetwork:      r.Network,
		},
	}, nil
}
