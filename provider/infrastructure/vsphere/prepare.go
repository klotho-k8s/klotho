package vsphere

import (
	"context"
	"net/netip"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Endpoint resolves the control plane VIP from the resolved configuration: the
// address must be a usable unicast IP (vSphere has no load balancer, so klotho
// does not allocate it), and the port is Port or DefaultPort.
func (r resolved) Endpoint() (cluster.Endpoint, error) {
	if r.EndpointIP == "" {
		return cluster.Endpoint{}, errorf("no control plane endpoint: vSphere has no load balancer, so set vsphere.endpointIP or the %s variable to the static VIP node zero's kube-vip will hold", VarEndpointIP)
	}
	ip, err := netip.ParseAddr(r.EndpointIP)
	if err != nil {
		return cluster.Endpoint{}, errorf("%s=%q is not an IP address", VarEndpointIP, r.EndpointIP)
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
		return cluster.Endpoint{}, errorf("%s=%q is not a usable unicast address", VarEndpointIP, r.EndpointIP)
	}
	return cluster.Endpoint{Host: ip.String(), Port: r.Port}, nil
}

// Prepare validates the configuration against vCenter and resolves the control
// plane endpoint. vSphere has no load balancer, so the endpoint must be a
// static VIP the user supplies (vsphere.endpointIP or CONTROL_PLANE_ENDPOINT_IP);
// Prepare only validates it, it never allocates, so Created is always empty. It
// injects the server, thumbprint, placement, template and identity as template
// variables so a generated bundle resolves to the same values this provider
// boots with.
func (p *Provider) Prepare(ctx context.Context, spec *cluster.Spec) (*infrastructure.Prepared, error) {
	if spec == nil || spec.Name == "" {
		return nil, errorf("spec has no cluster name")
	}
	r := p.resolve(spec.Variables)
	if r.Server == "" {
		return nil, errorf("no vcenter server: set vsphere.server or the %s variable", VarServer)
	}
	if r.Template == "" {
		return nil, errorf("no template: set vsphere.template or the %s variable", VarTemplate)
	}
	ep, err := r.Endpoint()
	if err != nil {
		return nil, err
	}

	exists, err := p.client.TemplateExists(ctx, r.Template)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errorf("template %q does not exist or is not marked as a template in datacenter %q", r.Template, r.Datacenter)
	}

	vars := map[string]string{
		VarServer:   r.Server,
		VarTemplate: r.Template,
	}
	setIf := func(key, val string) {
		if val != "" {
			vars[key] = val
		}
	}
	setIf(VarThumbprint, r.Thumbprint)
	setIf(VarDatacenter, r.Datacenter)
	setIf(VarDatastore, r.Datastore)
	setIf(VarFolder, r.Folder)
	setIf(VarResourcePool, r.ResourcePool)
	setIf(VarNetwork, r.Network)
	setIf(VarUsername, r.Username)
	setIf(VarPassword, r.Password)
	setIf(VarSSHKey, r.SSHKey)
	setIf(VarVIPInterface, r.VIPInterface)

	return &infrastructure.Prepared{
		Endpoint:  ep,
		Variables: vars,
	}, nil
}
