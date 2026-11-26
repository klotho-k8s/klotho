package cloudinit

import (
	"fmt"
	"net/netip"

	"sigs.k8s.io/yaml"
)

// NetworkConfig is a cloud-init network-config version 2 document, a subset
// of the netplan schema covering ethernet interfaces.
type NetworkConfig struct {
	Ethernets []Ethernet
}

// Ethernet configures one ethernet interface.
type Ethernet struct {
	// ID is the netplan key for the interface. When Match is empty it is
	// also the interface name. It defaults to eth<index>.
	ID string
	// Match selects the interface by MAC address or name; empty matches by ID.
	Match Match
	// SetName renames the matched interface.
	SetName     string
	DHCP4       bool
	Addresses   []string // CIDR, e.g. 192.0.2.10/24
	Gateway4    string
	Nameservers Nameservers
}

// Match selects an interface.
type Match struct {
	MACAddress string `json:"macaddress,omitempty"`
	Name       string `json:"name,omitempty"`
}

// Nameservers are the resolvers and search domains for an interface.
type Nameservers struct {
	Addresses []string `json:"addresses,omitempty"`
	Search    []string `json:"search,omitempty"`
}

type networkDoc struct {
	Version   int                    `json:"version"`
	Ethernets map[string]ethernetDoc `json:"ethernets"`
}

type ethernetDoc struct {
	Match       *Match       `json:"match,omitempty"`
	SetName     string       `json:"set-name,omitempty"`
	DHCP4       bool         `json:"dhcp4"`
	Addresses   []string     `json:"addresses,omitempty"`
	Gateway4    string       `json:"gateway4,omitempty"`
	Nameservers *Nameservers `json:"nameservers,omitempty"`
}

// Marshal validates the configuration and renders it as a version 2 document.
func (n *NetworkConfig) Marshal() ([]byte, error) {
	doc := networkDoc{Version: 2, Ethernets: map[string]ethernetDoc{}}
	for i, e := range n.Ethernets {
		id := e.ID
		if id == "" {
			id = fmt.Sprintf("eth%d", i)
		}
		if _, dup := doc.Ethernets[id]; dup {
			return nil, fmt.Errorf("cloudinit: network-config: duplicate ethernet id %q", id)
		}
		for _, a := range e.Addresses {
			if _, err := netip.ParsePrefix(a); err != nil {
				return nil, fmt.Errorf("cloudinit: network-config: ethernet %q address %q is not CIDR: %w", id, a, err)
			}
		}
		if e.Gateway4 != "" {
			gw, err := netip.ParseAddr(e.Gateway4)
			if err != nil || !gw.Is4() {
				return nil, fmt.Errorf("cloudinit: network-config: ethernet %q gateway4 %q is not an IPv4 address", id, e.Gateway4)
			}
		}
		for _, a := range e.Nameservers.Addresses {
			if _, err := netip.ParseAddr(a); err != nil {
				return nil, fmt.Errorf("cloudinit: network-config: ethernet %q nameserver %q is not an IP address", id, a)
			}
		}
		if !e.DHCP4 && len(e.Addresses) == 0 {
			return nil, fmt.Errorf("cloudinit: network-config: ethernet %q has neither dhcp4 nor addresses", id)
		}
		ed := ethernetDoc{
			SetName:   e.SetName,
			DHCP4:     e.DHCP4,
			Addresses: e.Addresses,
			Gateway4:  e.Gateway4,
		}
		if e.Match != (Match{}) {
			m := e.Match
			ed.Match = &m
		}
		if len(e.Nameservers.Addresses) > 0 || len(e.Nameservers.Search) > 0 {
			ns := e.Nameservers
			ed.Nameservers = &ns
		}
		doc.Ethernets[id] = ed
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("cloudinit: marshalling network-config: %w", err)
	}
	return out, nil
}
