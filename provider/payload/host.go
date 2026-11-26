// Package payload defines how a bootstrap node fetches files that are too
// large for a provider's user-data channel.
//
// # Why this exists
//
// klotho boots the first node of a cluster from cloud-init user-data that
// carries everything kubeadm and the Cluster API stack need: the cluster's
// certificate authorities, the admin kubeconfig, the kubeadm configuration,
// the static pod for the virtual IP, the scripts that install the CNI,
// cert-manager and the operator, and the Secrets the control plane
// controller expects to find. Every infrastructure provider delivers
// user-data through its own channel, and most channels have a hard size
// cap. When the payload does not fit, klotho publishes the large files
// through a Host and inlines only a small fetch command per file: the node
// retrieves each one at boot through a URL with a bounded lifetime, so the
// user-data still carries no credential.
//
// Providers whose channel has no practical size limit (a NoCloud ISO on
// Proxmox, guestinfo on vSphere) inline everything and never use a payload
// host, even when one is configured.
//
// # How big klotho's payload is
//
// Measured for a one-node kubeadm cluster on Proxmox
// (klotho create --dry-run <dir>; the user-data file in that directory):
//
//	user-data, raw                    43.7 KB
//	user-data, gzip                   25.8 KB
//	user-data, gzip then base64       about 34 KB
//	of which certificate material     about 30 KB (four CAs and the admin
//	                                  kubeconfig, shipped once as pki files
//	                                  and once as Cluster API Secrets)
//	cluster bundle (applied over the API, never in user-data)   8.7 KB
//
// The certificate material is the floor: RSA 2048 keys and certificates do
// not compress, and every control plane needs all of them. Everything a
// user adds to the control plane object grows the rest: extra files, a
// registry mirror configuration, certificate SANs, pre- and post-kubeadm
// commands.
//
// # Which providers need it
//
// User-data caps as documented by the providers (check the
// current limit before relying on a number):
//
//	AWS EC2                     16 KB       host required
//	Hetzner Cloud               32 KiB      host required (34 KB does not fit)
//	Azure, DigitalOcean,
//	OpenStack, Linode           64 KB       fits today with ~20 KB headroom;
//	                                        a customized control plane will
//	                                        need the host
//	GCP                         256 KB per metadata value; fits
//	Proxmox (NoCloud ISO),
//	vSphere (guestinfo)         no practical cap; never used
//
// Some providers count the base64-encoded size, so compare against the
// encoded figure, not the raw one.
//
// # Deciding for your own situation
//
// Run klotho create --dry-run <dir> with your real template and variables
// and look at the size of <dir>/user-data, raw and after gzip and base64.
// Compare it with your provider's documented cap, leaving room for the
// provider's own additions (network configuration, SSH keys) and for growth.
// If it does not fit, configure a payload host; the S3 implementation in the
// s3 subpackage works with AWS S3 and anything that speaks its API.
//
// # Security
//
// A published file is reachable by anyone holding its URL until the URL
// expires, so hosts must issue bounded-lifetime URLs and callers must remove
// the files once the node is up or the cluster is deleted. The files carry
// the cluster's private keys; the bucket should be private, and the URL
// lifetime should be as short as the boot allows.
package payload

import "context"

// File is one file the bootstrap node must have during cloud-init.
type File struct {
	// Path is where the file lands on the node.
	Path    string
	Content []byte
}

// Fetch tells the node how to retrieve one published file. The command must
// work with only what cloud-init has on a fresh machine and must not embed a
// long-lived credential; a bounded-lifetime URL is the expected shape.
type Fetch struct {
	Path    string
	Command string
}

// Host publishes files for a bootstrap node to fetch within a bounded window.
type Host interface {
	// Name is the host's identifier, as used by --payload.
	Name() string
	// Publish stores the files under the cluster's name and returns how the
	// node fetches each one.
	Publish(ctx context.Context, cluster string, files []File) ([]Fetch, error)
	// Remove deletes everything published for the cluster.
	Remove(ctx context.Context, cluster string) error
}
