package libvirt

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"net/url"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// libvirtdClient is the production Client: a connection to libvirtd over the
// libvirt wire protocol using github.com/digitalocean/go-libvirt (pure Go, no
// cgo). It is built by NewClient from a connection URI and talks to the same
// libvirtd the in-cluster Cluster API libvirt controller does.
type libvirtdClient struct {
	l    *golibvirt.Libvirt
	host string
}

// NewClient opens a connection to the libvirtd described by uri (for example
// "qemu:///system", "qemu+tcp://host/system" or "qemu+tls://host/system") and
// returns a Client talking to it. The host recorded in provider IDs is the
// URI's host, or "localhost" for a local socket.
func NewClient(uri string) (Client, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, errorf("parsing connection URI %q: %w", uri, err)
	}
	l, err := golibvirt.ConnectToURI(u)
	if err != nil {
		return nil, errorf("connecting to %q: %w", uri, err)
	}
	return &libvirtdClient{l: l, host: HostFromURI(uri)}, nil
}

func (c *libvirtdClient) Host() string { return c.host }

func (c *libvirtdClient) Close() error {
	if c.l == nil {
		return nil
	}
	return c.l.Disconnect()
}

func (c *libvirtdClient) GetDomain(_ context.Context, name string) (*Domain, error) {
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrDomainNotFound
		}
		return nil, errorf("looking up domain %q: %w", name, err)
	}
	return c.domainView(dom)
}

func (c *libvirtdClient) domainView(dom golibvirt.Domain) (*Domain, error) {
	state, _, err := c.l.DomainGetState(dom, 0)
	if err != nil {
		return nil, errorf("reading state of domain %q: %w", dom.Name, err)
	}
	desc := ""
	if x, err := c.l.DomainGetXMLDesc(dom, 0); err == nil {
		desc = descriptionFromXML(x)
	}
	return &Domain{
		Name:        dom.Name,
		UUID:        formatUUID(dom.UUID),
		State:       stateName(golibvirt.DomainState(state)),
		Description: desc,
	}, nil
}

func (c *libvirtdClient) CreateDomain(_ context.Context, in CreateDomainInput) (*Domain, error) {
	pool, err := c.l.StoragePoolLookupByName(in.StoragePool)
	if err != nil {
		return nil, errorf("looking up storage pool %q: %w", in.StoragePool, err)
	}
	_ = c.l.StoragePoolRefresh(pool, 0)

	base, err := c.l.StorageVolLookupByName(pool, in.BaseImage)
	if err != nil {
		return nil, errorf("looking up base image %q in pool %q: %w", in.BaseImage, in.StoragePool, err)
	}
	basePath, err := c.l.StorageVolGetPath(base)
	if err != nil {
		return nil, errorf("reading path of base image %q: %w", in.BaseImage, err)
	}
	capacity := in.DiskSizeBytes
	if capacity == 0 {
		if _, baseCap, _, err := c.l.StorageVolGetInfo(base); err == nil {
			capacity = int64(baseCap)
		}
	}

	overlayXML, err := overlayVolumeXML(in.OverlayName, basePath, capacity)
	if err != nil {
		return nil, err
	}
	overlayVol, err := c.l.StorageVolCreateXML(pool, overlayXML, 0)
	if err != nil {
		return nil, errorf("creating overlay volume %q: %w", in.OverlayName, err)
	}
	overlayPath, err := c.l.StorageVolGetPath(overlayVol)
	if err != nil {
		return nil, errorf("reading path of overlay %q: %w", in.OverlayName, err)
	}

	seedXML, err := rawVolumeXML(in.CIDataName, int64(len(in.CIData)))
	if err != nil {
		return nil, err
	}
	seedVol, err := c.l.StorageVolCreateXML(pool, seedXML, 0)
	if err != nil {
		return nil, errorf("creating cidata volume %q: %w", in.CIDataName, err)
	}
	if err := c.l.StorageVolUpload(seedVol, bytes.NewReader(in.CIData), 0, uint64(len(in.CIData)), 0); err != nil {
		return nil, errorf("uploading cidata seed %q: %w", in.CIDataName, err)
	}
	seedPath, err := c.l.StorageVolGetPath(seedVol)
	if err != nil {
		return nil, errorf("reading path of cidata %q: %w", in.CIDataName, err)
	}

	domXML, err := buildDomainXML(domainXMLInput{
		Name:             in.Name,
		UUID:             in.UUID,
		Description:      in.Description,
		MemoryBytes:      in.MemoryBytes,
		VCPU:             in.VCPU,
		OSDiskPath:       overlayPath,
		CloudInitISOPath: seedPath,
		NetworkName:      in.NetworkName,
		NetworkType:      in.NetworkType,
	})
	if err != nil {
		return nil, err
	}
	dom, err := c.l.DomainDefineXML(domXML)
	if err != nil {
		return nil, errorf("defining domain %q: %w", in.Name, err)
	}
	return &Domain{Name: dom.Name, UUID: formatUUID(dom.UUID), State: StateShutoff, Description: in.Description}, nil
}

func (c *libvirtdClient) StartDomain(_ context.Context, name string) error {
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		return errorf("looking up domain %q: %w", name, err)
	}
	if err := c.l.DomainCreate(dom); err != nil {
		return errorf("starting domain %q: %w", name, err)
	}
	return nil
}

func (c *libvirtdClient) DestroyDomain(_ context.Context, name string) error {
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return errorf("looking up domain %q: %w", name, err)
	}
	state, _, err := c.l.DomainGetState(dom, 0)
	if err == nil && golibvirt.DomainState(state) != golibvirt.DomainShutoff {
		if err := c.l.DomainDestroy(dom); err != nil && !isNotFound(err) {
			return errorf("destroying domain %q: %w", name, err)
		}
	}
	return nil
}

func (c *libvirtdClient) UndefineDomain(_ context.Context, name string) error {
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return errorf("looking up domain %q: %w", name, err)
	}
	// Delete the domain's own disks (the overlay and the cidata seed), found
	// from the domain XML. The base image is a qcow2 backing file, not a domain
	// disk source, so it is never touched.
	paths := diskSourcesFromXML(c.domainXML(dom))
	if state, _, err := c.l.DomainGetState(dom, 0); err == nil && golibvirt.DomainState(state) != golibvirt.DomainShutoff {
		_ = c.l.DomainDestroy(dom)
	}
	if err := c.l.DomainUndefine(dom); err != nil && !isNotFound(err) {
		return errorf("undefining domain %q: %w", name, err)
	}
	for _, path := range paths {
		if vol, err := c.l.StorageVolLookupByPath(path); err == nil {
			_ = c.l.StorageVolDelete(vol, 0)
		}
	}
	return nil
}

func (c *libvirtdClient) GetDomainIPs(_ context.Context, name string) ([]string, error) {
	dom, err := c.l.DomainLookupByName(name)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrDomainNotFound
		}
		return nil, errorf("looking up domain %q: %w", name, err)
	}
	// Prefer the qemu-guest-agent, which reports addresses as soon as the guest
	// is up; fall back to the network's DHCP leases.
	for _, src := range []golibvirt.DomainInterfaceAddressesSource{
		golibvirt.DomainInterfaceAddressesSrcAgent,
		golibvirt.DomainInterfaceAddressesSrcLease,
	} {
		ifaces, err := c.l.DomainInterfaceAddresses(dom, uint32(src), 0)
		if err != nil {
			continue
		}
		if addrs := addressesOf(ifaces); len(addrs) > 0 {
			return addrs, nil
		}
	}
	return nil, nil
}

func (c *libvirtdClient) ListDomains(_ context.Context, prefix string) ([]*Domain, error) {
	doms, _, err := c.l.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, errorf("listing domains: %w", err)
	}
	var out []*Domain
	for _, dom := range doms {
		if !strings.HasPrefix(dom.Name, prefix) {
			continue
		}
		v, err := c.domainView(dom)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (c *libvirtdClient) ListNetworks(_ context.Context) ([]string, error) {
	nets, _, err := c.l.ConnectListAllNetworks(1, 0)
	if err != nil {
		return nil, errorf("listing networks: %w", err)
	}
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.Name)
	}
	return out, nil
}

func (c *libvirtdClient) ListStoragePools(_ context.Context) ([]string, error) {
	pools, _, err := c.l.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return nil, errorf("listing storage pools: %w", err)
	}
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.Name)
	}
	return out, nil
}

func (c *libvirtdClient) VolumeExists(_ context.Context, pool, vol string) (bool, error) {
	p, err := c.l.StoragePoolLookupByName(pool)
	if err != nil {
		if isNotFound(err) {
			return false, errorf("storage pool %q does not exist", pool)
		}
		return false, errorf("looking up storage pool %q: %w", pool, err)
	}
	_ = c.l.StoragePoolRefresh(p, 0)
	if _, err := c.l.StorageVolLookupByName(p, vol); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, errorf("looking up volume %q in pool %q: %w", vol, pool, err)
	}
	return true, nil
}

func (c *libvirtdClient) domainXML(dom golibvirt.Domain) string {
	x, err := c.l.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return ""
	}
	return x
}

// --- helpers ---------------------------------------------------------------

// isNotFound reports whether err is libvirt's "not found" error. go-libvirt
// surfaces it as an *golibvirt.Error with a code; the code for the common
// "domain not found" / "storage volume not found" family is matched by
// message, which is stable across versions.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no domain") ||
		strings.Contains(msg, "no storage vol") || strings.Contains(msg, "no network")
}

func formatUUID(u golibvirt.UUID) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

func stateName(s golibvirt.DomainState) string {
	switch s {
	case golibvirt.DomainRunning:
		return StateRunning
	case golibvirt.DomainShutoff:
		return StateShutoff
	case golibvirt.DomainPaused:
		return StatePaused
	case golibvirt.DomainShutdown:
		return StateShutdown
	case golibvirt.DomainCrashed:
		return StateCrashed
	default:
		return StateNostate
	}
}

// addressesOf collects the usable IP addresses from interface reports, skipping
// loopback and link-local addresses.
func addressesOf(ifaces []golibvirt.DomainInterface) []string {
	var out []string
	for _, iface := range ifaces {
		for _, a := range iface.Addrs {
			ip := net.ParseIP(a.Addr)
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, a.Addr)
		}
	}
	return out
}

// descriptionFromXML extracts the domain's <description> text.
func descriptionFromXML(domXML string) string {
	var d struct {
		Description string `xml:"description"`
	}
	if err := xml.Unmarshal([]byte(domXML), &d); err != nil {
		return ""
	}
	return strings.TrimSpace(d.Description)
}

// diskSourcesFromXML returns the host file paths of a domain's disk sources
// (the overlay and the cidata seed).
func diskSourcesFromXML(domXML string) []string {
	var d struct {
		Devices struct {
			Disks []struct {
				Source struct {
					File string `xml:"file,attr"`
				} `xml:"source"`
			} `xml:"disk"`
		} `xml:"devices"`
	}
	if err := xml.Unmarshal([]byte(domXML), &d); err != nil {
		return nil
	}
	var out []string
	for _, disk := range d.Devices.Disks {
		if disk.Source.File != "" {
			out = append(out, disk.Source.File)
		}
	}
	return out
}
