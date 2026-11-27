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
| Azure | CAPZ | by name (pre-set ok) | internal LB (forced) | ✅ | 🟡 **help wanted** | 🚧 finisher TODO | — |
| Apple `vz` | CAPVZ | by name / `providerID` | node-zero IP | ✅ adapter | 🟡 awaiting Mac run | 🧹 daemon finisher (no deadlock) | macOS / Apple Silicon |

*Planned next:* vSphere (CAPV), OpenStack (CAPO), Hetzner (CAPH).

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

### Azure (CAPZ) — help wanted
Adopts node zero by name; a pre-set `providerID` is fine (CAPL-style). Uses an internal
API-server LB plus node zero's own public IP as the endpoint, a BYO resource
group/vnet/subnet, and an `AzureClusterIdentity` service principal. Build + unit green;
**never run live** (no Azure credentials in the dev environment). See
`provider/infrastructure/azure/TESTING.md`.

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

### Apple Virtualization (CAPVZ) — scaffold
See [cluster-api-provider-vz](https://github.com/klotho-k8s/cluster-api-provider-vz). A
host-side gRPC daemon supervises one `capvz-runner` process per VM (launchd-managed), so
the daemon can restart without killing VMs. macOS/Apple-Silicon only; not yet integrated
into a klotho adapter.

## Help wanted — test on your environment

If you have access to any of these, you can move a provider from 🟡 to ✅:

- **GCP** — a GCP project with Compute access. Run `provider/infrastructure/gcp/TESTING.md`.
- **Azure** — a subscription + a service principal. Run `provider/infrastructure/azure/TESTING.md`.
- **Linode** — an API token. Run `provider/infrastructure/linode/TESTING.md`.
- **vSphere / OpenStack / Hetzner** — provider work is planned; if you'd run the live test,
  tell us and we'll prioritize that provider.

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
