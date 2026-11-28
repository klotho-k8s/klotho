# Infrastructure Provider Support

klotho bootstraps **self-managed Cluster API clusters** — it boots node zero directly
(no kind, no separate management cluster), installs the CAPI operator plus an
infrastructure provider, adopts node zero, and pivots to self-managed. This page tracks
how far each infrastructure provider has been taken and, just as importantly, **where it
has actually been tested on real infrastructure**.

> **Maintainers and users: we want your environment.** Several providers are implemented
> and pass build + unit tests but have never run against live infrastructure, because we
> don't have access to every cloud/hypervisor. If you run one of the environments below,
> please run the provider's `TESTING.md` recipe and report back (see
> [How to report results](#how-to-report-results)). The gaps marked 🟡 are the ones we
> most need help with.

## Status legend

**Implementation**
- ✅ Implemented, and run end-to-end on live infrastructure
- 🟡 Implemented, build + unit tests green, **not yet run live — help wanted**
- 🧪 Experimental / in progress
- ❌ Not implemented yet

**Node-zero adoption** — how CAPI takes over the hand-booted first node
- *by name* — CAPI matches the VM/instance by name (klotho must not pre-set `providerID`)
- *by providerID* — klotho pre-sets `spec.providerID` and CAPI adopts that resource

**Teardown** — klotho's answer to the self-hosted deletion deadlock (the controller runs
on the last control-plane node it must delete, so it can't issue the final destroy from
inside)
- 🧹 Clean, hands-off `klotho delete` to zero, proven live
- ⚠️ Reaches zero, but not hands-off (needs `--force` / manual cleanup today)
- 🚧 External finisher not built yet
- — not applicable

## Support matrix

| Provider | CAP | Adoption | Endpoint / LB | Impl. | Live-tested | Teardown | Tested on |
|---|---|---|---|---|---|---|---|
| Proxmox | CAPMOX | by name | kube-vip / user-supplied | ✅ | ✅ | 🧹 LXC sidecar | Proxmox VE 9.x home lab |
| Docker | CAPD | by name + label | — (local) | ✅ | ✅ | 🧹 cleanup container + `--watch` | podman (Windows/WSL) |
| AWS | CAPA | by tag (no pre-set id) | $0 option (LB disabled) | ✅ | ✅ create / ⚠️ delete | ⚠️ reaches zero, not hands-off | AWS account |
| Hyper-V | CAPHV | by name | node-zero IP | ✅ | ✅ create → pivot → clean delete | 🧹 host-side finisher (daemon untouched) | Windows 11, single node |
| libvirt / KVM | CAPLV | by name | node-zero IP | ✅ | ✅ create → pivot → clean delete | 🧹 destroy + undefine (libvirtd external, no sidecar) | WSL2 nested KVM |
| Linode | CAPL | by `providerID` | $0 option (NodeBalancer optional) | ✅ | 🟡 live run pending | 🚧 finisher TODO | — |
| GCP | CAPG | by name | managed LB (forced) | ✅ | 🟡 **help wanted** | 🚧 finisher TODO | — |
| Hetzner | CAPH | by name (no pre-set id) | $0 option (LB disabled) | ✅ | 🟡 **help wanted** | 🚧 finisher TODO | — |
| DigitalOcean | CAPDO | by `providerID` | managed LB (forced) | ✅ | 🟡 **help wanted** | 🚧 finisher TODO | — |
| OpenStack | CAPO | by name | $0 option (floating IP, no Octavia) | ✅ | 🟡 **help wanted** | 🚧 finisher TODO | — |
| Azure | CAPZ | by name (pre-set ok) | internal LB (forced) | ✅ | 🟡 **help wanted** | 🚧 finisher TODO | — |
| OCI | CAPOCI | by display-name + freeform tags | managed NLB skippable / user-supplied | ✅ | 🟡 **help wanted** | 🚧 finisher TODO | — |
| Apple `vz` | CAPVZ | by name / `providerID` | node-zero IP | ✅ adapter | 🟡 awaiting Mac run | 🧹 daemon finisher (no deadlock) | macOS / Apple Silicon |
| vSphere | CAPV | by `providerID` | kube-vip / user-supplied | ✅ | 🟡 **help wanted** | 🧹 govmomi external finisher (no sidecar) | — |

## Provider notes

### Proxmox (CAPMOX) — reference implementation
Adopts the running node-zero VM by name (no duplicate clone, no power cycle). Talks to the
Proxmox API through [go-proxmox](https://github.com/luthermonson/go-proxmox) with a
least-privilege API token (not root). Teardown is a tiny unprivileged Alpine LXC sidecar
that outlives the cluster and finishes the destroy the in-cluster controller can't. This is
the most exercised path — multiple clean create/delete cycles on a live Proxmox VE 9.x
cluster.

### Docker (CAPD)
Uses the real upstream CAPD controller (klotho binds the Docker socket into it); the only
thing skipped versus `clusterctl` is the kind cluster. Teardown is a Docker-in-Docker
cleanup container plus an in-process `--watch` mode; 100% teardown proven on podman.

### AWS (CAPA)
Adopts node zero by tag — klotho must **not** pre-set `providerID` (CAPA fails a machine
whose id is set but whose instance isn't found). A cost-safe template runs with no NAT and
the control-plane LB disabled. Create + adopt + pivot are proven live; **delete is not yet
hands-off** — the current run reached zero via `klotho delete --force` plus manual
security-group / bucket cleanup. The external-API teardown finisher is the outstanding work.

### Hyper-V (CAPHV) — experimental, single Windows node
See [cluster-api-provider-hyperv](https://github.com/klotho-k8s/cluster-api-provider-hyperv).
The in-cluster controller (Linux) drives a host-side gRPC daemon over TLS + token, because
Hyper-V's only API is local Windows WMI. The full CAPI provider stack has been brought up
healthy on a live host; unattended pivot and a clean, VM-destroying delete are in progress.
Intended for single-Windows-node **development** clusters only.

### Linode (CAPL)
Opposite of AWS: klotho **pre-sets** `spec.providerID` and CAPL adopts that. Implemented
with build + unit coverage; a live create/delete on a real Linode account is pending.

### GCP (CAPG) — help wanted
Adopts node zero by name. Stock CAPG always provisions a managed external load balancer
(an unavoidable ~$20–30/mo standing cost); klotho reserves the same global IP so the
endpoint is known before cert-minting. Build + unit green; **never run live** (no GCP
credentials in the dev environment). See `provider/infrastructure/gcp/TESTING.md`.

### Hetzner Cloud (CAPH) — help wanted
Adopts node zero by name (hcloud variant, not the bare-metal Robot path): klotho
boots a `<cluster>-bootstrap` Hetzner Cloud server with its cloud-init in the
server's `user_data`, and CAPH adopts it by name — on reconcile CAPH tries to
create a server of the same name, hits Hetzner's `uniqueness_error`, and resolves
it by finding and adopting the existing server, then setting `providerID`
(`hcloud://<id>`) itself. So klotho must **not** pre-set `spec.providerID` (the
GCP/CAPD model). The cost-safe flat template disables CAPH's managed load balancer
(`controlPlaneLoadBalancer.enabled: false`) and the private network, so the only
standing cost is the servers; klotho supplies the control plane endpoint (a DNS
name you point at node zero). No cloud controller manager — a `preKubeadmCommand`
reads the numeric server ID from Hetzner's instance-metadata service and sets the
kubelet `--provider-id=hcloud://<id>` directly. klotho reaches hcloud with the
official Go SDK ([`hcloud-go`](https://github.com/hetznercloud/hcloud-go)) using an
`HCLOUD_TOKEN`. Build + unit green; **never run live** (no Hetzner credentials in
the dev environment). See `provider/infrastructure/hetzner/TESTING.md`.

### DigitalOcean (CAPDO) — help wanted
Like Linode: klotho **pre-sets** `spec.providerID` (`digitalocean://<droplet-id>`)
and CAPDO adopts that droplet — CAPDO's only droplet lookup is by the ID parsed
from `spec.providerID`, and with none set it creates a duplicate, so the pre-set
ID is load-bearing. Stock CAPDO always fronts the API server with a managed
DigitalOcean load balancer (an unavoidable ~$12/mo standing cost) and overwrites
`controlPlaneEndpoint` with its IP; klotho creates that load balancer up front so
the endpoint is known before cert-minting and injects its UUID
(`spec.network.apiServerLoadbalancers.resourceId`) so CAPDO adopts it rather than
provisioning a second. Node zero is tagged with CAPDO's apiserver role tag so the
load balancer forwards to it. Build + unit green; **never run live** (no
DigitalOcean credentials in the dev environment). See
`provider/infrastructure/digitalocean/TESTING.md`.

### OpenStack (CAPO) — help wanted
Adopts node zero **by name** — klotho must **not** pre-set `providerID`. CAPO's
`OpenStackServer` reconciler, when it has no recorded instance ID, calls
`GetInstanceStatusByName(name)`, which lists Nova servers with an anchored-exact
name filter (`^<name>$`): exactly one match is reused (adopted) and its UUID
becomes the machine's `providerID`; zero creates a new server; more than one is a
`DuplicateServerNames` error. The Nova server name equals the OpenStackServer
object name equals the OpenStackMachine object name, so klotho boots node zero as
`<cluster>-bootstrap` and CAPO finds and reuses it. `spec.providerID` is
output-only (CAPO overwrites it to `openstack:///<uuid>` once ACTIVE), so klotho
leaves it unset. Confirmed against CAPO v0.15.2 source
(`controllers/openstackserver_controller.go` `getOrCreateServer`,
`pkg/cloud/services/compute/instance.go` `GetInstanceStatusByName`).

The endpoint is cost-aware with **no Octavia load balancer**: klotho allocates a
Neutron floating IP from the external network in `Prepare` (so the endpoint is
known before cert-minting), boots node zero, and associates the floating IP with
it; the flat template disables the managed LB and keeps that floating IP
(`spec.apiServer.floatingIP`) after adoption. Set `openstack.controlPlaneEndpoint`
to an existing floating IP / Octavia VIP / DNS name to bring your own instead
(nothing is allocated). The kubelet is given `openstack:///<nova-uuid>` directly,
so there is no cloud controller manager. klotho talks to Nova/Neutron/Glance with
the official `github.com/gophercloud/gophercloud/v2` SDK, authenticating from a
`clouds.yaml` entry (`openstack.cloud` / `OS_CLOUD`); that clouds.yaml is handed,
base64-encoded, to the in-cluster controller as a per-cluster credential Secret.
Build + unit green; **never run live** (no OpenStack in the dev environment —
self-hostable via DevStack/MicroStack). See
`provider/infrastructure/openstack/TESTING.md`. Teardown reaches zero (servers +
floating IP) but is not yet a hands-off finisher.

### Azure (CAPZ) — help wanted
Adopts node zero by name; a pre-set `providerID` is fine (CAPL-style). Uses an internal
API-server LB plus node zero's own public IP as the endpoint, a BYO resource
group/vnet/subnet, and an `AzureClusterIdentity` service principal. Build + unit green;
**never run live** (no Azure credentials in the dev environment). See
`provider/infrastructure/azure/TESTING.md`.

### OCI (CAPOCI) — help wanted
Adopts node zero by **display name cross-checked against CAPOCI's own ownership freeform
tags**. CAPOCI's machine reconciler finds an existing instance by listing the compartment
filtered to the OCIMachine's name and accepts it only when it carries
`CreatedBy=OCIClusterAPIProvider` and `ClusterResourceIdentifier=<OCICluster.spec.ociResourceIdentifier>`.
So klotho names the bootstrap instance `<cluster>-bootstrap`, pins `ociResourceIdentifier`
to the cluster name in the flat template, and stamps both CAPOCI tags (plus klotho's own
`klotho.io/cluster` ownership tag) on node zero before boot. `spec.providerID` is an output
(`oci://<instance-ocid>`) CAPOCI sets itself — klotho must **not** pre-set it (the GCP/CAPD
model, opposite of Linode). Cloud-init is delivered through the instance metadata `user_data`
key (base64), the same channel CAPOCI uses. Stock CAPOCI always fronts the API server with a
managed OCI load balancer (NLB by default); klotho's flat template instead sets the
`cluster.x-k8s.io/skip-apiserver-lb-management` annotation and a user-supplied
`controlPlaneEndpoint`, so no NLB is provisioned and the endpoint klotho minted the
certificates for stands (point it at node zero's own reserved public IP for a single node,
or an NLB you manage for HA). The cluster uses an **existing VCN + subnet** (networkSpec with
`vcn.skip: true`), the same one node zero booted into, so CAPOCI adopts in place. Host-side
auth is the OCI SDK config file (`~/.oci/config`) or instance principal; the in-cluster
controller uses an `OCIClusterIdentity`/instance principal set up out of band. Build + unit
green; **never run live** (no OCI credentials in the dev environment). Teardown reaches zero
by terminating tagged instances but is not yet hands-off (finisher TODO, like GCP/Linode).
See `provider/infrastructure/oci/TESTING.md`. The OCI Always Free tier (Ampere A1.Flex) or
trial credits are enough to run the recipe.

### libvirt / KVM (CAPLV) — experimental
Adopts node zero by name: klotho boots a `<cluster>-bootstrap` libvirt domain (a qcow2
overlay of a base cloud image with a NoCloud cidata seed on a NAT network), and the
Cluster API libvirt provider adopts it by `spec.domainName`. klotho reaches libvirtd
directly over the connection URI (`qemu:///system`, `qemu+tcp`, `qemu+tls`) with the
pure-Go [go-libvirt](https://github.com/digitalocean/go-libvirt) (no cgo). libvirt has no
load balancer, so the endpoint is node zero's own address (`CONTROL_PLANE_ENDPOINT_HOST`),
and `providerID` is `libvirt://<host>/<domain-uuid>`. Because libvirtd is reachable from
outside, teardown is the clean external finisher — klotho's own `Teardown` destroys and
undefines the domains and deletes the overlay/cidata volumes (the base image is untouched),
so no deletion sidecar is needed. Build + unit green; **not run live here** (no libvirtd in
the dev environment). See `provider/infrastructure/libvirt/TESTING.md` for a WSL2 nested-KVM
runbook and a remote `qemu+tls` (Proxmox Linux VM) alternative.

### Apple Virtualization (CAPVZ) — experimental
See [cluster-api-provider-vz](https://github.com/klotho-k8s/cluster-api-provider-vz). A
host-side gRPC daemon (`capvz-daemon`) supervises one `capvz-runner` process per VM
(launchd-managed), so the daemon can restart without killing VMs. macOS/Apple-Silicon only.
Adopts node zero by name: klotho boots a `<cluster>-bootstrap` vz VM through the daemon (the
daemon copies a raw arm64 base image and builds a NoCloud cidata seed on the Mac from the
bootstrap user-data klotho sends over `AttachCloudInit`), and the Cluster API vz provider
adopts it by `spec.vmName`. klotho reaches the daemon over gRPC with TLS and a bearer token
(the same transport the in-cluster controller uses), in a pure-Go client with no cgo. vz has
no load balancer, so the endpoint is node zero's own address (`CONTROL_PLANE_ENDPOINT_HOST`),
and `providerID` is `vz://<host>/<vmid>`. Because the daemon runs on the host, not inside the
cluster, teardown is the clean external finisher — klotho's own `Teardown` force-stops and
removes the VMs through the daemon (the base image is untouched), so no deletion sidecar is
needed. Build + unit green on any OS (pure Go); **not yet run live** (needs an Apple-Silicon
Mac). See `provider/infrastructure/vz/TESTING.md` for the Mac build/sign/run steps.

### vSphere (CAPV) — help wanted
Uses the upstream govmomi-based Cluster API provider for vSphere (CAPV), not the
supervisor/vm-operator variant. klotho boots a `<cluster>-bootstrap` VM by cloning
a vSphere template through the vCenter API with the official Go SDK
[govmomi](https://github.com/vmware/govmomi), delivering the bootstrap user-data
through the VMware **guestinfo** cloud-init datasource (`guestinfo.userdata` /
`guestinfo.metadata`, base64), and reads the VM's BIOS UUID for the provider ID
`vsphere://<uuid>`. vSphere has no cloud load balancer, so the control-plane
endpoint is a static VIP the user supplies (`CONTROL_PLANE_ENDPOINT_IP`), realised
by a kube-vip static pod on node zero. The flat single-node template runs **no
vSphere CPI/CSI**: the kubelet self-registers its provider ID from the guestinfo
metadata instance-id, so the Machine<->Node ref completes with no cloud controller
manager. Because vCenter is reachable from outside the cluster (like Proxmox),
teardown is the clean external finisher — klotho's own `Teardown` powers off and
deletes the VMs through govmomi (the template is untouched), so no deletion
sidecar is needed.

**Adoption is `providerID`-based (CAPL-style), and unverified live.** CAPV has no
field to adopt a pre-existing VM by name — its only native create path is
clone-from-template, and it identifies its VM by the BIOS UUID behind the provider
ID (confirmed against the `infrastructure.cluster.x-k8s.io/v1beta2` govmomi-mode
CRDs on CAPV `main`). So klotho pre-sets the `VSphereMachine`'s `spec.providerID`
to the running VM's `vsphere://<uuid>`, intending CAPV to adopt it rather than
clone a duplicate. This is **not** an officially documented CAPV feature and MUST
be confirmed on a live vCenter — see `provider/infrastructure/vsphere/TESTING.md`.
Build + unit green; **never run live** (no vCenter in the dev environment; `vcsim`
runs no real VMs, so it cannot exercise the lifecycle).

## Help wanted — test on your environment

If you have access to any of these, you can move a provider from 🟡 to ✅:

- **GCP** — a GCP project with Compute access. Run `provider/infrastructure/gcp/TESTING.md`.
- **Azure** — a subscription + a service principal. Run `provider/infrastructure/azure/TESTING.md`.
- **Linode** — an API token. Run `provider/infrastructure/linode/TESTING.md`.
- **Hetzner Cloud** — an hcloud API token and an SSH key in the project. Run
  `provider/infrastructure/hetzner/TESTING.md`.
- **vSphere** — a vCenter + ESXi host. Run `provider/infrastructure/vsphere/TESTING.md`
  (confirm CAPV adopts node zero rather than cloning a duplicate — the unverified path).
- **DigitalOcean** — an API token. Run `provider/infrastructure/digitalocean/TESTING.md`.
- **OpenStack** — any OpenStack cloud (a free local DevStack or MicroStack works). Run
  `provider/infrastructure/openstack/TESTING.md`.
- **OCI** — an Oracle Cloud tenancy (the Always Free tier / trial credits suffice). Run
  `provider/infrastructure/oci/TESTING.md`.

For each run, the three things we care about:
1. Does the cluster come up (node zero → CAPI stack Ready)?
2. Does it **pivot to self-managed**?
3. Does `klotho delete` reach **zero with no dangling resources**?

## How to report results

- **Quickest:** open a PR editing this file — update the provider's `Live-tested` and
  `Tested on` columns, and add a dated line to its notes (e.g. *"2026-11-30: full
  create/pivot/delete on GCP us-central1, e2-medium — clean."*).
- **Or** open an issue with the provider's `TESTING.md` checklist output, your environment
  (versions, region/host), and anything that needed a workaround.

Partial results are welcome — "came up and pivoted but delete left an orphan LB" is exactly
the kind of signal that closes gaps.
