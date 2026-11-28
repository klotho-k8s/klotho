package openstack

import (
	"context"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
)

// Server is the subset of a Nova server klotho reasons about.
type Server struct {
	ID        string
	Name      string
	Status    string
	Addresses []string
	Tags      []string
}

// FloatingIP is the subset of a Neutron floating IP klotho reasons about.
type FloatingIP struct {
	ID      string
	Address string
	PortID  string
}

// CreateServerOpts are the inputs to CreateServer, with names already resolved
// to the IDs Nova needs.
type CreateServerOpts struct {
	Name             string
	FlavorID         string
	ImageID          string
	NetworkID        string
	SSHKeyName       string
	SecurityGroups   []string
	AvailabilityZone string
	UserData         []byte
	Tags             []string
}

// ComputeAPI is the subset of OpenStack (Nova, Neutron, Glance) klotho uses,
// reduced to plain request/response methods so it can be faked in tests.
// openstackClient, built by NewClient, satisfies it with gophercloud.
type ComputeAPI interface {
	// CreateServer boots a Nova server and returns it.
	CreateServer(ctx context.Context, opts CreateServerOpts) (*Server, error)
	// GetServer returns one server by ID (a gophercloud 404 when it does not
	// exist).
	GetServer(ctx context.Context, id string) (*Server, error)
	// DeleteServer deletes one server by ID.
	DeleteServer(ctx context.Context, id string) error
	// ListServersByTag returns every server carrying the given Nova tag.
	ListServersByTag(ctx context.Context, tag string) ([]Server, error)

	// FlavorIDByName resolves a Nova flavor name to its ID.
	FlavorIDByName(ctx context.Context, name string) (string, error)
	// ImageIDByName resolves a Glance image name to its ID.
	ImageIDByName(ctx context.Context, name string) (string, error)
	// NetworkIDByName resolves a Neutron network name to its ID.
	NetworkIDByName(ctx context.Context, name string) (string, error)
	// SubnetExists reports whether a subnet with the given name exists on the
	// network.

	// AllocateFloatingIP allocates a floating IP from the external network.
	AllocateFloatingIP(ctx context.Context, externalNetworkID, description string) (*FloatingIP, error)
	// GetFloatingIP returns one floating IP by ID.
	GetFloatingIP(ctx context.Context, id string) (*FloatingIP, error)
	// AssociateFloatingIP associates a floating IP with a server's first port.
	AssociateFloatingIP(ctx context.Context, floatingIPID, serverID string) error
	// DeleteFloatingIP releases a floating IP by ID.
	DeleteFloatingIP(ctx context.Context, id string) error
}

// openstackClient is the gophercloud-backed ComputeAPI.
type openstackClient struct {
	compute *gophercloud.ServiceClient
	network *gophercloud.ServiceClient
	image   *gophercloud.ServiceClient
}

// NewClient authenticates against the named cloud in clouds.yaml (resolved from
// cloudsYAML, or the standard OpenStack locations when empty) and builds the
// Nova, Neutron and Glance service clients. region overrides the clouds.yaml
// region when set. It makes the authentication call but no resource calls.
func NewClient(ctx context.Context, cloud, cloudsYAML, region string) (ComputeAPI, error) {
	if cloud == "" {
		return nil, errorf("cloud is required (set openstack.cloud, the clouds.yaml entry)")
	}
	if cloudsYAML != "" {
		// clientconfig honours OS_CLIENT_CONFIG_FILE for a non-standard path.
		if err := os.Setenv("OS_CLIENT_CONFIG_FILE", cloudsYAML); err != nil {
			return nil, errorf("setting clouds.yaml path: %w", err)
		}
	}
	opts := &clientconfig.ClientOpts{Cloud: cloud, RegionName: region}

	compute, err := clientconfig.NewServiceClient(ctx, "compute", opts)
	if err != nil {
		return nil, errorf("building the Nova client: %w", err)
	}
	// Server tags on create and filtering servers by tag need microversion 2.52.
	compute.Microversion = "2.52"
	network, err := clientconfig.NewServiceClient(ctx, "network", opts)
	if err != nil {
		return nil, errorf("building the Neutron client: %w", err)
	}
	image, err := clientconfig.NewServiceClient(ctx, "image", opts)
	if err != nil {
		return nil, errorf("building the Glance client: %w", err)
	}
	return &openstackClient{compute: compute, network: network, image: image}, nil
}

func toServer(s *servers.Server) *Server {
	out := &Server{ID: s.ID, Name: s.Name, Status: s.Status}
	if s.Tags != nil {
		out.Tags = *s.Tags
	}
	for _, v := range s.Addresses {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, a := range list {
			m, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if addr, ok := m["addr"].(string); ok && addr != "" {
				out.Addresses = append(out.Addresses, addr)
			}
		}
	}
	return out
}

func (c *openstackClient) CreateServer(ctx context.Context, opts CreateServerOpts) (*Server, error) {
	base := servers.CreateOpts{
		Name:             opts.Name,
		FlavorRef:        opts.FlavorID,
		ImageRef:         opts.ImageID,
		UserData:         opts.UserData,
		SecurityGroups:   opts.SecurityGroups,
		AvailabilityZone: opts.AvailabilityZone,
		Networks:         []servers.Network{{UUID: opts.NetworkID}},
		Tags:             opts.Tags,
	}
	var builder servers.CreateOptsBuilder = base
	if opts.SSHKeyName != "" {
		builder = keypairs.CreateOptsExt{CreateOptsBuilder: base, KeyName: opts.SSHKeyName}
	}
	srv, err := servers.Create(ctx, c.compute, builder, nil).Extract()
	if err != nil {
		return nil, err
	}
	return toServer(srv), nil
}

func (c *openstackClient) GetServer(ctx context.Context, id string) (*Server, error) {
	srv, err := servers.Get(ctx, c.compute, id).Extract()
	if err != nil {
		return nil, err
	}
	return toServer(srv), nil
}

func (c *openstackClient) DeleteServer(ctx context.Context, id string) error {
	return servers.Delete(ctx, c.compute, id).ExtractErr()
}

func (c *openstackClient) ListServersByTag(ctx context.Context, tag string) ([]Server, error) {
	pages, err := servers.List(c.compute, servers.ListOpts{Tags: tag}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	list, err := servers.ExtractServers(pages)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(list))
	for i := range list {
		out = append(out, *toServer(&list[i]))
	}
	return out, nil
}

func (c *openstackClient) FlavorIDByName(ctx context.Context, name string) (string, error) {
	pages, err := flavors.ListDetail(c.compute, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	all, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return "", err
	}
	for _, f := range all {
		if f.Name == name {
			return f.ID, nil
		}
	}
	return "", errorf("flavor %q not found", name)
}

func (c *openstackClient) ImageIDByName(ctx context.Context, name string) (string, error) {
	pages, err := images.List(c.image, images.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	all, err := images.ExtractImages(pages)
	if err != nil {
		return "", err
	}
	for _, img := range all {
		if img.Name == name {
			return img.ID, nil
		}
	}
	return "", errorf("image %q not found", name)
}

func (c *openstackClient) NetworkIDByName(ctx context.Context, name string) (string, error) {
	pages, err := networks.List(c.network, networks.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	all, err := networks.ExtractNetworks(pages)
	if err != nil {
		return "", err
	}
	for _, n := range all {
		if n.Name == name {
			return n.ID, nil
		}
	}
	return "", errorf("network %q not found", name)
}

func (c *openstackClient) AllocateFloatingIP(ctx context.Context, externalNetworkID, description string) (*FloatingIP, error) {
	fip, err := floatingips.Create(ctx, c.network, floatingips.CreateOpts{
		FloatingNetworkID: externalNetworkID,
		Description:       description,
	}).Extract()
	if err != nil {
		return nil, err
	}
	return &FloatingIP{ID: fip.ID, Address: fip.FloatingIP, PortID: fip.PortID}, nil
}

func (c *openstackClient) GetFloatingIP(ctx context.Context, id string) (*FloatingIP, error) {
	fip, err := floatingips.Get(ctx, c.network, id).Extract()
	if err != nil {
		return nil, err
	}
	return &FloatingIP{ID: fip.ID, Address: fip.FloatingIP, PortID: fip.PortID}, nil
}

func (c *openstackClient) AssociateFloatingIP(ctx context.Context, floatingIPID, serverID string) error {
	pages, err := ports.List(c.network, ports.ListOpts{DeviceID: serverID}).AllPages(ctx)
	if err != nil {
		return err
	}
	portList, err := ports.ExtractPorts(pages)
	if err != nil {
		return err
	}
	var portID string
	for i := range portList {
		if len(portList[i].FixedIPs) > 0 {
			portID = portList[i].ID
			break
		}
	}
	if portID == "" {
		return errorf("server %s has no port with a fixed IP to associate a floating IP with", serverID)
	}
	_, err = floatingips.Update(ctx, c.network, floatingIPID, floatingips.UpdateOpts{PortID: &portID}).Extract()
	return err
}

func (c *openstackClient) DeleteFloatingIP(ctx context.Context, id string) error {
	return floatingips.Delete(ctx, c.network, id).ExtractErr()
}

// notFound reports whether err is a gophercloud 404.
func notFound(err error) bool {
	return gophercloud.ResponseCodeIs(err, 404)
}
