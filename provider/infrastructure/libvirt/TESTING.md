# Testing the klotho libvirt provider

The provider is fully unit-tested against a fake libvirt client (`go test
./provider/infrastructure/libvirt/...`), with no running libvirtd required. This
document is the live-run runbook for a real create -> pivot -> delete against a
libvirt/KVM host.

klotho reaches libvirtd directly over the connection URI, and libvirtd is
reachable from outside the cluster, so there is **no deletion sidecar**: klotho's
own `Teardown` finishes the last node from the host. The in-cluster Cluster API
libvirt controller reaches the same libvirtd over the same URI.

## What the provider does on a live run

1. **Prepare** resolves and validates: the storage pool and base image exist,
   the libvirt network exists, and `CONTROL_PLANE_ENDPOINT_HOST` is set (libvirt
   has no load balancer, so the endpoint is the single control-plane node's own
   address — a static address or a DHCP reservation on the NAT network). It
   allocates nothing and injects `LIBVIRT_URI`, `LIBVIRT_HOST`,
   `CONTROL_PLANE_ENDPOINT_HOST`, `LIBVIRT_IMAGE`, `LIBVIRT_POOL` and
   `LIBVIRT_NETWORK` into generation.
2. **Launch** generates the domain UUID, defines `<cluster>-bootstrap` from a
   qcow2 overlay of the base image with a NoCloud cidata seed (meta-data
   `instance-id` = the UUID), starts it, and waits for the qemu-guest-agent to
   report an IPv4 address. The node's provider ID is
   `libvirt://<host>/<uuid>`.
3. **Adopt** renders the `LibvirtMachine` with `spec.domainName =
   <cluster>-bootstrap` so the provider adopts the running domain by name. It
   does **not** pre-set `spec.providerID`; the controller sets it, and because
   the kubelet `--provider-id` is `libvirt://<host>/{{ ds.meta_data.instance_id
   }}` and the instance-id is the domain UUID, the IDs agree and the
   Machine<->Node nodeRef completes.
4. **Teardown** destroys and undefines each `<cluster>-*` domain that carries the
   `klotho-<cluster>` tag in its description, deleting the overlay and cidata
   volumes with it. **The base image and the storage pool are never touched.**

## Primary target: WSL2 with nested KVM

A WSL2 Ubuntu distro with nested virtualization is the cheapest way to exercise
the full flow on a Windows host.

### 1. Enable nested virtualization for WSL2

In `%UserProfile%\.wslconfig` on Windows:

```ini
[wsl2]
nestedVirtualization=true
```

Then `wsl --shutdown` and reopen the distro. Confirm `/dev/kvm` exists and is
usable:

```bash
ls -l /dev/kvm
sudo apt-get install -y cpu-checker && sudo kvm-ok   # "KVM acceleration can be used"
```

If `/dev/kvm` is missing, nested virtualization is not active (check the host CPU
virtualization is enabled in firmware, and that you are on WSL2 not WSL1:
`wsl -l -v`).

### 2. Install libvirt + QEMU

```bash
sudo apt-get update
sudo apt-get install -y libvirt-daemon-system qemu-kvm virtinst qemu-utils libguestfs-tools
sudo systemctl enable --now libvirtd
sudo usermod -aG libvirt,kvm "$USER"   # re-login (or: exec su -l "$USER") for the group to take effect
virsh version
```

### 3. The `default` NAT network

libvirt ships a `default` NAT network (subnet `192.168.122.0/24`) that hands out
DHCP leases and reaches the internet — exactly what a dev cluster needs.

```bash
sudo virsh net-list --all
sudo virsh net-start default      # if not already active
sudo virsh net-autostart default
```

Pick a control-plane endpoint address **outside** the DHCP range so it is stable.
Either reserve a fixed lease for the bootstrap MAC, or just choose a free address
in the subnet the node will hold; it is set with `CONTROL_PLANE_ENDPOINT_HOST`.

### 4. A base cloud image in a storage pool

Use the `default` storage pool (`/var/lib/libvirt/images`) and drop a cloud-init
(NoCloud) enabled qcow2 cloud image into it as a **volume** (the provider backs a
thin overlay off it and never modifies it):

```bash
sudo virsh pool-list --all
sudo virsh pool-start default && sudo virsh pool-autostart default   # if needed

# Example: Debian 12 generic cloud image (cloud-init ready).
cd /tmp
curl -LO https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
sudo cp debian-12-genericcloud-amd64.qcow2 /var/lib/libvirt/images/debian-12.qcow2
sudo virsh pool-refresh default
sudo virsh vol-list default        # "debian-12.qcow2" must appear
```

Stock cloud images carry no Kubernetes, so create the cluster with
`--install-kubernetes` (or `nodes.installKubernetes: true`) so containerd and the
chosen Kubernetes version are installed on the node before kubeadm runs.

### 5. The connection URI

Local, on the same WSL2 distro klotho runs in:

```
qemu:///system
```

(`HostFromURI` maps this to `localhost`, which becomes the `<host>` in provider
IDs.) Confirm it works: `virsh -c qemu:///system list --all`.

### 6. Configure klotho and run

`~/.config/klotho/config.yaml`:

```yaml
backend: file
libvirt:
  uri: qemu:///system
  storagePool: default
  image: debian-12.qcow2
  network: default
  endpointHost: 192.168.122.10    # the address node zero will hold
providers:
  libvirt:
    version: v0.1.0
    # libvirt is not in clusterctl's built-in provider list, so point these at a
    # release (or a local clusterctl repository) carrying its
    # infrastructure-components.yaml + metadata.yaml + cluster-template.yaml:
    fetchURL: https://github.com/klotho-k8s/cluster-api-provider-libvirt/releases/v0.1.0/infrastructure-components.yaml
```

```bash
klotho init
klotho create --provider libvirt --control-plane kubeadm \
  --kubernetes-version v1.33.4 --install-kubernetes lab1
# ... cluster comes up, pivots self-managed ...
klotho delete lab1
```

Verify by hand at each stage:

```bash
virsh list --all                       # lab1-bootstrap Running after Launch
virsh dumpxml lab1-bootstrap | grep -E 'description|uuid|source file'
virsh domifaddr lab1-bootstrap         # guest-agent address == endpoint plan
# after delete: no lab1-* domains, overlay/cidata volumes gone, base image intact
virsh vol-list default
```

### Note on a flat clusterctl repository (unpublished provider)

Because `libvirt` is not a clusterctl built-in, `providers.libvirt.fetchURL`
(and, for the in-cluster operator, `componentsURL`) must resolve the provider's
components. For local iteration, build a clusterctl-layout directory and point
`fetchURL` at a `file://` URL of it (it must contain `infrastructure-components.yaml`,
`metadata.yaml`, and `cluster-template.yaml`), mirroring the pattern in the other
providers' TESTING docs. The operator inside the cluster needs an `https`/`oci`
URL it can reach for `componentsURL`.

## Alternative target: a Linux VM on Proxmox, remote `qemu+tls`

When WSL2 nested KVM is unavailable, run libvirtd on a Linux VM (for example on
the Proxmox lab) and have klotho reach it remotely over TLS.

### On the libvirt host (the Linux VM)

```bash
sudo apt-get install -y libvirt-daemon-system qemu-kvm
# Enable the TLS listener in /etc/libvirt/libvirtd.conf:
#   listen_tls = 1
#   listen_tcp = 0
# and start libvirtd with --listen (edit the service / default file):
#   /etc/default/libvirtd:  LIBVIRTD_ARGS="--listen"
```

Set up the libvirt TLS PKI (CA + server cert for the host, client cert for
klotho) under `/etc/pki/...` per the libvirt TLS guide
(`https://libvirt.org/kbase/tlscerts.html`). The TLS port is 16514. Ensure the
VM has nested KVM (`/dev/kvm`), the `default` NAT network, and a base-image
volume in a pool, exactly as in the WSL2 steps above.

### On the klotho host

Install the client certificate/key and the CA into libvirt's default PKI paths
(`~/.pki/libvirt/` or `/etc/pki/libvirt/`), or pass `?pkipath=...` in the URI,
then set:

```yaml
libvirt:
  uri: qemu+tls://<host>/system      # HostFromURI -> <host>, used in provider IDs
  storagePool: default
  image: debian-12.qcow2
  network: default
  endpointHost: <an address node zero will hold on the host's NAT network>
```

The in-cluster Cluster API libvirt controller is handed the same `LIBVIRT_URI`;
for `qemu+tls` it reads the client PKI from a `secretRef` on the `LibvirtCluster`
(`cacert.pem`, `clientcert.pem`, `clientkey.pem`) — provision that secret per the
provider's docs. Note the control-plane node's address must be routable from the
klotho host for the post-pivot API calls.
