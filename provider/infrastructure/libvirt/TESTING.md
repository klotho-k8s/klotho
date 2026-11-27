# Testing the klotho libvirt provider

The provider is fully unit-tested against a fake libvirt client (`go test
./provider/infrastructure/libvirt/...`), with no running libvirtd required. This
document is the live-run runbook for a real create -> pivot -> delete against a
libvirt/KVM host.

klotho reaches libvirtd directly over the connection URI, and libvirtd is
reachable from outside the cluster, so there is **no deletion sidecar**: klotho's
own `Teardown` finishes the last node from the host. The in-cluster Cluster API
libvirt controller reaches the **same libvirtd** - but from a pod, so it needs a
**network URI** (`qemu+tcp`/`qemu+tls`), not the host's local socket. klotho the
CLI may use `qemu:///system` (`libvirt.uri`) while the controller uses
`libvirt.controllerURI` (see section 5); both point at one libvirtd, and the
controller URI's host is what provider IDs are built from.

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

On newer Ubuntu (26.04 and later) `qemu-kvm` is a transitional/virtual package
that may not pull the emulator in; install the arch package explicitly:

```bash
sudo apt-get install -y qemu-system-x86   # provides qemu-system-x86_64
```

### 3. The `default` NAT network

libvirt ships a `default` NAT network (subnet `192.168.122.0/24`) that hands out
DHCP leases and reaches the internet — exactly what a dev cluster needs.

```bash
sudo virsh net-list --all
sudo virsh net-start default      # if not already active
sudo virsh net-autostart default
```

**Pinning node zero's address (the single-address DHCP-reservation trick).**
libvirt has no load balancer, so the control-plane endpoint
(`CONTROL_PLANE_ENDPOINT_HOST`) is node zero's own address and must be stable.
The simplest way to guarantee the bootstrap domain gets exactly that address is
to shrink the `default` network's DHCP pool to a **single address** equal to the
endpoint, so the only lease libvirt can hand out is the one you want:

```bash
# Make the whole DHCP range one address (start == end), e.g. 192.168.122.10.
sudo virsh net-update default modify ip-dhcp-range \
  "<range start='192.168.122.10' end='192.168.122.10'/>" --live --config
sudo virsh net-destroy default && sudo virsh net-start default   # apply to a running net
sudo virsh net-dumpxml default | grep range   # confirm start==end==endpointHost
```

Set `libvirt.endpointHost` (or `CONTROL_PLANE_ENDPOINT_HOST`) to that same
address. (A MAC-bound `<host>` reservation works too, but the single-address
range needs no knowledge of the domain's MAC in advance.)

### 4. A base cloud image in a storage pool

Use the `default` storage pool (`/var/lib/libvirt/images`) and drop a cloud-init
(NoCloud) enabled qcow2 cloud image into it as a **volume** (the provider backs a
thin overlay off it and never modifies it):

Use the **Ubuntu 24.04 (noble) cloud image**. It ships cloud-init and boots the
NoCloud seed reliably. (Prefer it over the Debian 12 generic cloud image, which
in the current build ships **without** cloud-init and so never consumes the
cidata seed - the node comes up with no SSH key, no hostname and no kubeadm.)

```bash
sudo virsh pool-list --all
sudo virsh pool-start default && sudo virsh pool-autostart default   # if needed

# Ubuntu 24.04 LTS (noble) server cloud image, cloud-init ready.
cd /tmp
curl -LO https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
sudo cp noble-server-cloudimg-amd64.img /var/lib/libvirt/images/ubuntu-24.qcow2
sudo virsh pool-refresh default
sudo virsh vol-list default        # "ubuntu-24.qcow2" must appear
```

Stock cloud images carry no Kubernetes, so create the cluster with
`--install-kubernetes` (or `nodes.installKubernetes: true`) so containerd and the
chosen Kubernetes version are installed on the node before kubeadm runs.

### 5. The connection URI: local socket for klotho, `qemu+tcp` for the controller

There are **two** consumers of libvirtd, and they do not share a network
position:

- **klotho the CLI** runs on the WSL2 host and can use the local socket
  `qemu:///system` (`libvirt.uri`). Confirm it: `virsh -c qemu:///system list --all`.
- **The in-cluster Cluster API libvirt controller** runs in a pod on node zero (a
  libvirt domain on `192.168.122.0/24`). A **unix socket is unreachable from a
  pod**, so the controller must be given a **network URI** it can dial:
  `qemu+tcp://<host>/system` (or `qemu+tls://...`). The host the pod can reach is
  the libvirt bridge gateway, `192.168.122.1` (virbr0). Set this as
  `libvirt.controllerURI`.

klotho injects `libvirt.controllerURI` as the `LibvirtCluster.spec.connectionURI`
and as the provider's `LIBVIRT_URI` secret, and derives `LIBVIRT_HOST` (and its
own provider IDs) from that URI's host, so the controller, the kubelet's
`--provider-id`, and klotho all agree. If you leave `controllerURI` empty it
falls back to `uri`; `uri: qemu:///system` alone is rejected by `Prepare` with a
clear error, since the controller could not reach it.

**Enable the libvirtd TCP listener** (dev-only, no TLS) so `qemu+tcp` works:

```bash
# /etc/libvirt/libvirtd.conf
sudo tee -a /etc/libvirt/libvirtd.conf >/dev/null <<'EOF'
listen_tls = 0
listen_tcp = 1
tcp_port = "16509"
listen_addr = "0.0.0.0"
auth_tcp = "none"
EOF

# libvirtd must be started with --listen. On WSL2 (systemd) use the socket+flag:
sudo mkdir -p /etc/systemd/system/libvirtd.service.d
sudo tee /etc/systemd/system/libvirtd.service.d/override.conf >/dev/null <<'EOF'
[Service]
Environment=LIBVIRTD_ARGS=--listen
EOF
sudo systemctl daemon-reload
sudo systemctl restart libvirtd
# Confirm the listener and that the gateway URI works end to end:
ss -tlnp | grep 16509
virsh -c qemu+tcp://192.168.122.1/system list --all
```

(TLS instead of plain TCP is the production shape; set `listen_tls = 1`, provision
the PKI per the Proxmox/`qemu+tls` section below, and use
`controllerURI: qemu+tls://192.168.122.1/system`.)

### 6. Serving the CAPI and provider components over http (ConfigMap embedding)

The in-cluster Cluster API **operator** installs each provider from a
`fetchConfig` and only accepts `github`/`gitlab`/`oci`/`local` schemes for a URL -
it **rejects a plain `http` URL** ("no provider implementation for \"http\"
schema"). The libvirt provider is unpublished (no GitHub release), and in a
self-contained WSL run the CAPI core components are served from a local mirror
too, so both would be `http`.

klotho handles this itself: when `capi.fetchURL` or the provider's
`componentsURL` is a plain-`http` URL, klotho **downloads the components (and the
sibling `metadata.yaml`) on the host and embeds them in labeled ConfigMaps**,
pointing each provider's `fetchConfig.selector` at its ConfigMap instead of a
URL. The operator reads components from a ConfigMap on any scheme, so nothing is
fetched externally except the manager container images (pulled from public
GHCR). This covers the **CoreProvider, Bootstrap, ControlPlane and the libvirt
InfrastructureProvider** alike.

Serve the component files over http from the host (reachable from the pods at the
bridge gateway `192.168.122.1`):

```bash
# Lay out a webroot with the CAPI release files and the provider components.
mkdir -p /root/web/capi /root/web/libvirt
# CAPI v1.12.9 release assets (core/bootstrap/control-plane components + metadata):
for f in core-components.yaml bootstrap-components.yaml control-plane-components.yaml metadata.yaml; do
  curl -Lo /root/web/capi/$f \
    https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.12.9/$f
done
# libvirt provider components + metadata (from the provider build):
#   /root/web/libvirt/infrastructure-components.yaml
#   /root/web/libvirt/metadata.yaml
( cd /root/web && python3 -m http.server 8099 --bind 0.0.0.0 ) &
```

`~/.config/klotho/config.yaml`:

```yaml
backend: file
encryption:
  plain: true
libvirt:
  uri: qemu:///system                           # klotho the CLI dials the local socket
  controllerURI: qemu+tcp://192.168.122.1/system # the in-pod controller dials the gateway
  storagePool: default
  image: ubuntu-24.qcow2
  network: default
  endpointHost: 192.168.122.10                   # == the single-address DHCP reservation
  diskGiB: 20
  memoryMiB: 6144
  cpus: 4
nodes:
  installKubernetes: true
capi:
  version: v1.12.9
  fetchURL: http://192.168.122.1:8099/capi/      # http -> klotho embeds core/bootstrap/cp in ConfigMaps
  cniManifestURL: https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml
providers:
  libvirt:
    version: v0.1.0
    # fetchURL is the host-side template/components read (a file:// clusterctl
    # repo is fine); componentsURL is what the operator would fetch - http here,
    # so klotho embeds it in a ConfigMap instead.
    fetchURL: file:///root/caplv/infrastructure-libvirt/v0.1.0/infrastructure-components.yaml
    componentsURL: http://192.168.122.1:8099/libvirt/infrastructure-components.yaml
    # The provider image: the clusterctl template references :v0.1.0, which does
    # not exist on GHCR. Override to a published tag (latest, or the sha tag).
    image: ghcr.io/klotho-k8s/cluster-api-provider-libvirt:sha-39126d4
```

```bash
klotho init
klotho create --provider libvirt --control-plane kubeadm \
  --kubernetes-version v1.33.4 --install-kubernetes lab1
# ... cluster comes up, pivots self-managed ...
klotho delete lab1
```

Confirm the providers installed from ConfigMaps (not URLs) and are Ready:

```bash
kubectl --kubeconfig <bundle-kubeconfig> get providers -A
kubectl get configmaps -A -l provider.cluster.x-k8s.io/name   # the embedded components
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
providers' TESTING docs. For `componentsURL` the operator accepts an
`https`/`oci` URL directly; a plain-`http` URL is **also fine** because klotho
embeds those components in a ConfigMap on the host before applying (section 6),
so a local `http` mirror needs no TLS.

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

Here `uri` is already a network URI, so `controllerURI` can be left empty (it
falls back to `uri`) and the controller is handed that `qemu+tls://<host>/system`;
for `qemu+tls` the controller reads the client PKI from a `secretRef` on the `LibvirtCluster`
(`cacert.pem`, `clientcert.pem`, `clientkey.pem`) — provision that secret per the
provider's docs. Note the control-plane node's address must be routable from the
klotho host for the post-pivot API calls.
