package libvirt

import (
	"crypto/rand"
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cloudinit"
)

// metaData is the minimal NoCloud meta-data document. instance-id must be the
// domain UUID so the kubelet --provider-id "libvirt://<host>/{{
// ds.meta_data.instance_id }}" resolves to the adopted machine's provider ID.
type metaData struct {
	InstanceID    string `json:"instance-id"`
	LocalHostname string `json:"local-hostname"`
}

// buildCIData renders the NoCloud cidata seed image (an iso9660 "CIDATA"
// filesystem) carrying userData verbatim and meta-data whose instance-id is the
// domain UUID and whose local-hostname is the domain name. No network-config is
// written, so the image's default applies (DHCP on the first NIC).
func buildCIData(uuid, hostname string, userData []byte) ([]byte, error) {
	if len(userData) == 0 {
		return nil, errorf("empty user-data")
	}
	if uuid == "" {
		return nil, errorf("empty instance-id (domain UUID)")
	}
	md, err := yaml.Marshal(metaData{InstanceID: uuid, LocalHostname: hostname})
	if err != nil {
		return nil, errorf("building meta-data: %w", err)
	}
	iso, err := cloudinit.ISO(&cloudinit.Bundle{UserData: userData, MetaData: md})
	if err != nil {
		return nil, errorf("building cidata seed: %w", err)
	}
	return iso, nil
}

// newUUID returns a random RFC 4122 version 4 UUID as the canonical
// 8-4-4-4-12 string.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
