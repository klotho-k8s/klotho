package libvirt

import (
	"encoding/xml"
	"fmt"
)

// DefaultEmulatorPath is the usual path to the x86_64 QEMU system emulator on a
// libvirt host. It is emitted into the domain XML.
const DefaultEmulatorPath = "/usr/bin/qemu-system-x86_64"

// domainXMLInput is the set of fields needed to render a domain. Paths are
// paths on the libvirt host, resolved by the client from storage volumes.
type domainXMLInput struct {
	Name             string
	UUID             string
	Description      string
	MemoryBytes      int64
	VCPU             int
	OSDiskPath       string
	CloudInitISOPath string
	NetworkName      string
	NetworkType      string
}

// buildDomainXML validates the input and returns the domain XML document for a
// KVM/QEMU guest: a virtio qcow2 OS disk (a per-VM overlay), a read-only
// NoCloud cidata CD-ROM, a virtio NIC on a libvirt network or host bridge, a
// qemu-guest-agent channel for in-guest IP discovery, virtio rng and balloon,
// and a serial/pty console.
func buildDomainXML(in domainXMLInput) (string, error) {
	switch {
	case in.Name == "":
		return "", fmt.Errorf("domain xml: name must not be empty")
	case in.MemoryBytes <= 0:
		return "", fmt.Errorf("domain xml: memoryBytes must be > 0")
	case in.VCPU <= 0:
		return "", fmt.Errorf("domain xml: vcpu must be > 0")
	case in.OSDiskPath == "":
		return "", fmt.Errorf("domain xml: osDiskPath must not be empty")
	}

	netType := in.NetworkType
	if netType == "" {
		netType = "network"
	}
	netName := in.NetworkName
	if netName == "" {
		netName = "default"
	}
	src := ifaceSource{}
	switch netType {
	case "network":
		src.Network = netName
	case "bridge":
		src.Bridge = netName
	default:
		return "", fmt.Errorf("domain xml: unknown network type %q (want network or bridge)", netType)
	}

	dom := domainDoc{
		Type:          "kvm",
		Name:          in.Name,
		UUID:          in.UUID,
		Description:   in.Description,
		Memory:        memory{Unit: "KiB", Value: in.MemoryBytes / 1024},
		CurrentMemory: memory{Unit: "KiB", Value: in.MemoryBytes / 1024},
		VCPU:          vcpu{Placement: "static", Value: in.VCPU},
		OS: osSpec{
			Type: osType{Arch: "x86_64", Machine: "q35", Value: "hvm"},
			Boot: bootDev{Dev: "hd"},
		},
		Features:   features{ACPI: &empty{}, APIC: &empty{}},
		CPU:        cpuSpec{Mode: "host-passthrough"},
		Clock:      clock{Offset: "utc"},
		OnPoweroff: "destroy",
		OnReboot:   "restart",
		OnCrash:    "restart",
		Devices: devices{
			Emulator: DefaultEmulatorPath,
			Disks: []disk{{
				Type:   "file",
				Device: "disk",
				Driver: diskDriver{Name: "qemu", Type: "qcow2"},
				Source: diskSource{File: in.OSDiskPath},
				Target: diskTarget{Dev: "vda", Bus: "virtio"},
			}},
			Interface: iface{
				Type:   netType,
				Source: src,
				Model:  ifaceModel{Type: "virtio"},
			},
			Channel: channel{
				Type:   "unix",
				Target: channelTarget{Type: "virtio", Name: "org.qemu.guest_agent.0"},
			},
			Serial:     &devPty{Type: "pty"},
			Console:    &devPty{Type: "pty"},
			RNG:        rng{Model: "virtio", Backend: rngBackend{Model: "random", Value: "/dev/urandom"}},
			MemBalloon: memBalloon{Model: "virtio"},
		},
	}

	if in.CloudInitISOPath != "" {
		dom.Devices.Disks = append(dom.Devices.Disks, disk{
			Type:     "file",
			Device:   "cdrom",
			Driver:   diskDriver{Name: "qemu", Type: "raw"},
			Source:   diskSource{File: in.CloudInitISOPath},
			Target:   diskTarget{Dev: "sda", Bus: "sata"},
			ReadOnly: &empty{},
		})
	}

	out, err := xml.MarshalIndent(dom, "", "  ")
	if err != nil {
		return "", fmt.Errorf("domain xml: marshaling domain: %w", err)
	}
	return xml.Header + string(out) + "\n", nil
}

// overlayVolumeXML returns the storage volume XML for a thin qcow2 overlay
// backed by backingPath (the base cloud image). capacityBytes is the overlay's
// virtual size; zero omits <capacity> so the overlay inherits the backing
// image's size.
func overlayVolumeXML(name, backingPath string, capacityBytes int64) (string, error) {
	if name == "" || backingPath == "" {
		return "", fmt.Errorf("overlay volume: needs a name and a backing path")
	}
	v := storageVolDoc{
		Name:         name,
		Target:       volTarget{Format: volFormat{Type: "qcow2"}},
		BackingStore: &backingStore{Path: backingPath, Format: volFormat{Type: "qcow2"}},
	}
	if capacityBytes > 0 {
		v.Capacity = &capacity{Unit: "bytes", Value: capacityBytes}
	}
	return marshalVol(v)
}

// rawVolumeXML returns the storage volume XML for a raw volume of
// capacityBytes, used for the cloud-init cidata seed (an iso9660 image uploaded
// into it).
func rawVolumeXML(name string, capacityBytes int64) (string, error) {
	if name == "" {
		return "", fmt.Errorf("raw volume: needs a name")
	}
	v := storageVolDoc{
		Name:     name,
		Capacity: &capacity{Unit: "bytes", Value: capacityBytes},
		Target:   volTarget{Format: volFormat{Type: "raw"}},
	}
	return marshalVol(v)
}

func marshalVol(v storageVolDoc) (string, error) {
	out, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling volume: %w", err)
	}
	return string(out) + "\n", nil
}

// --- XML model -------------------------------------------------------------

type storageVolDoc struct {
	XMLName      xml.Name      `xml:"volume"`
	Name         string        `xml:"name"`
	Capacity     *capacity     `xml:"capacity"`
	Target       volTarget     `xml:"target"`
	BackingStore *backingStore `xml:"backingStore"`
}

type capacity struct {
	Unit  string `xml:"unit,attr"`
	Value int64  `xml:",chardata"`
}

type volTarget struct {
	Format volFormat `xml:"format"`
}

type volFormat struct {
	Type string `xml:"type,attr"`
}

type backingStore struct {
	Path   string    `xml:"path"`
	Format volFormat `xml:"format"`
}

type domainDoc struct {
	XMLName       xml.Name `xml:"domain"`
	Type          string   `xml:"type,attr"`
	Name          string   `xml:"name"`
	UUID          string   `xml:"uuid,omitempty"`
	Description   string   `xml:"description,omitempty"`
	Memory        memory   `xml:"memory"`
	CurrentMemory memory   `xml:"currentMemory"`
	VCPU          vcpu     `xml:"vcpu"`
	OS            osSpec   `xml:"os"`
	Features      features `xml:"features"`
	CPU           cpuSpec  `xml:"cpu"`
	Clock         clock    `xml:"clock"`
	OnPoweroff    string   `xml:"on_poweroff"`
	OnReboot      string   `xml:"on_reboot"`
	OnCrash       string   `xml:"on_crash"`
	Devices       devices  `xml:"devices"`
}

type memory struct {
	Unit  string `xml:"unit,attr"`
	Value int64  `xml:",chardata"`
}

type vcpu struct {
	Placement string `xml:"placement,attr,omitempty"`
	Value     int    `xml:",chardata"`
}

type osSpec struct {
	Type osType  `xml:"type"`
	Boot bootDev `xml:"boot"`
}

type osType struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}

type bootDev struct {
	Dev string `xml:"dev,attr"`
}

type features struct {
	ACPI *empty `xml:"acpi"`
	APIC *empty `xml:"apic"`
}

type cpuSpec struct {
	Mode string `xml:"mode,attr"`
}

type clock struct {
	Offset string `xml:"offset,attr"`
}

type empty struct{}

type devices struct {
	Emulator   string     `xml:"emulator"`
	Disks      []disk     `xml:"disk"`
	Interface  iface      `xml:"interface"`
	Channel    channel    `xml:"channel"`
	Serial     *devPty    `xml:"serial"`
	Console    *devPty    `xml:"console"`
	RNG        rng        `xml:"rng"`
	MemBalloon memBalloon `xml:"memballoon"`
}

type disk struct {
	Type     string     `xml:"type,attr"`
	Device   string     `xml:"device,attr"`
	Driver   diskDriver `xml:"driver"`
	Source   diskSource `xml:"source"`
	Target   diskTarget `xml:"target"`
	ReadOnly *empty     `xml:"readonly"`
}

type diskDriver struct {
	Name string `xml:"name,attr"`
	Type string `xml:"type,attr"`
}

type diskSource struct {
	File string `xml:"file,attr"`
}

type diskTarget struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type iface struct {
	Type   string      `xml:"type,attr"`
	Source ifaceSource `xml:"source"`
	Model  ifaceModel  `xml:"model"`
}

type ifaceSource struct {
	Network string `xml:"network,attr,omitempty"`
	Bridge  string `xml:"bridge,attr,omitempty"`
}

type ifaceModel struct {
	Type string `xml:"type,attr"`
}

type channel struct {
	Type   string        `xml:"type,attr"`
	Target channelTarget `xml:"target"`
}

type channelTarget struct {
	Type string `xml:"type,attr"`
	Name string `xml:"name,attr"`
}

type devPty struct {
	Type string `xml:"type,attr"`
}

type rng struct {
	Model   string     `xml:"model,attr"`
	Backend rngBackend `xml:"backend"`
}

type rngBackend struct {
	Model string `xml:"model,attr"`
	Value string `xml:",chardata"`
}

type memBalloon struct {
	Model string `xml:"model,attr"`
}
