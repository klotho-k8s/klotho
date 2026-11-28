# Testing the klotho vSphere provider

The provider is fully unit-tested against a fake vCenter client (`go test
./provider/infrastructure/vsphere/...`), with no reachable vCenter required. This
document is the live-run runbook for a real create -> pivot -> delete against a
vCenter + ESXi environment.

> **Status: help wanted.** This provider is implemented and passes build + unit
> tests, but has **never been run against a live vCenter** — the dev environment
> has none. In particular the node-zero **adoption model is not yet verified
> live** (see the warning below). If you have a vCenter, running this recipe and
> reporting back (even a partial result) is exactly the signal that moves the
> provider from 🟡 to ✅ in `docs/PROVIDERS.md`.

klotho reaches vCenter directly over the govmomi SDK, and vCenter is reachable
from outside the cluster (like the Proxmox API), so there is **no deletion
sidecar**: klotho's own `Teardown` powers off and deletes the VMs from the host.
The in-cluster CAPV controller reaches the **same vCenter** through the
per-cluster identity `Secret` the flat template carries.

## ⚠️ vcsim does not work for a full run

`govmomi`'s simulator (`vcsim`) models the vCenter API but **runs no real VMs**:
a cloned "VM" never boots, never runs cloud-init, and never reports a guest IP,
so `Launch` (which waits for an IPv4 address) and the whole pivot cannot be
exercised against it. vcsim is useful only for exercising the govmomi client's
request/response plumbing, not the cluster lifecycle. A live run needs a real
**vCenter + at least one ESXi host**.

## ⚠️ Node-zero adoption is unverified (the part most needing a live run)

CAPV has **no field to adopt a pre-existing VM by name** — its only native create
path is clone-from-template, and it identifies its VM by the BIOS UUID behind the
provider ID. klotho therefore adopts node zero the **Linode (CAPL) way**: it
pre-sets the `VSphereMachine`'s `spec.providerID` to the running VM's
`vsphere://<bios-uuid>`, intending CAPV to reconcile the existing VM rather than
clone a duplicate. **This is not an officially documented CAPV feature.** The
single most important thing to confirm on a live run is: *after Adopt + pivot,
does CAPV adopt klotho's `<cluster>-bootstrap` VM, or does it clone a second
control-plane VM?* If it clones a duplicate, the adoption approach needs
revisiting (e.g. pre-creating the `VSphereVM` with `spec.biosUUID`). Please
report what you observe.

## What the provider does on a live run

1. **Prepare** resolves and validates: `VSPHERE_SERVER` and `VSPHERE_TEMPLATE`
   are set, the template exists and is marked as a template in vCenter, and
   `CONTROL_PLANE_ENDPOINT_IP` is a usable unicast VIP (vSphere has no load
   balancer; kube-vip holds the VIP on node zero). It allocates nothing and
   injects `VSPHERE_SERVER`, `VSPHERE_TLS_THUMBPRINT`, `VSPHERE_DATACENTER`,
   `VSPHERE_DATASTORE`, `VSPHERE_FOLDER`, `VSPHERE_RESOURCE_POOL`,
   `VSPHERE_NETWORK`, `VSPHERE_TEMPLATE`, `VSPHERE_USERNAME`, `VSPHERE_PASSWORD`
   and `VSPHERE_SSH_AUTHORIZED_KEY` into generation.
2. **Launch** clones the template into `<cluster>-bootstrap` (powered off),
   reads the BIOS UUID vCenter assigned, writes the bootstrap user-data and
   cloud-init metadata through the VMware **guestinfo** datasource
   (`guestinfo.userdata` / `guestinfo.metadata`, base64, `instance-id` = the BIOS
   UUID), powers the VM on, and waits for VMware Tools to report an IPv4 address.
   The node's provider ID is `vsphere://<bios-uuid>`. The VM carries the
   `klotho-<cluster>` tag in its annotation.
3. **Adopt** renders the `VSphereMachine` named `<cluster>-bootstrap` with
   `spec.providerID = vsphere://<bios-uuid>` pre-set (see the adoption warning
   above). The flat template runs **no vSphere CPI/CSI**: the kubelet
   self-registers the same provider ID from `{{ ds.meta_data.instance_id }}`, so
   the Machine<->Node nodeRef completes with no cloud controller manager.
4. **Teardown** powers off and deletes each `<cluster>-*` VM that carries the
   `klotho-<cluster>` annotation tag. **The template is never touched.**

## Prerequisites

- A **vCenter Server** and at least one **ESXi** host with capacity for a 2-vCPU,
  8 GiB control-plane VM.
- A **VM template** with cloud-init and the VMware guestinfo datasource enabled
  (for example, an Ubuntu 22.04/24.04 cloud image imported as a template, with
  `cloud-init` and `open-vm-tools` installed, and `datasource_list: [VMware,
  OVF, NoCloud]` so the guestinfo datasource is used). If the template carries no
  Kubernetes, create the cluster with `--install-kubernetes`.
- A free **static IP** on the VM network for the control-plane VIP (kube-vip).
- A least-privilege **vCenter account** (clone, power, reconfigure, destroy on
  the target datacenter/datastore/folder/resource pool), not the root
  `administrator@vsphere.local` if avoidable.
- The vCenter TLS **thumbprint** (SHA-1), unless you accept insecure TLS:
  `openssl s_client -connect vcenter.example.com:443 < /dev/null 2>/dev/null |
  openssl x509 -noout -fingerprint -sha1`.

## 1. Configure klotho

In `config.yaml` (or a profile):

```yaml
provider: vsphere
vsphere:
  server: vcenter.example.com          # or https://vcenter.example.com/sdk
  username: klotho@vsphere.local       # or set the VSPHERE_USERNAME env var
  # password: set the VSPHERE_PASSWORD env var; do not store it here
  thumbprint: "AA:BB:CC:..."           # SHA-1 of the vCenter cert
  # insecure: true                     # alternative to a thumbprint (dev only)
  datacenter: dc1
  datastore: datastore1
  folder: klotho                       # a VM folder; empty = datacenter default
  resourcePool: "*/Resources"          # empty = datacenter default
  network: "VM Network"
  template: ubuntu-2404-template       # a cloud-init + guestinfo template
  cloneMode: fullClone                 # or linkedClone (template needs a snapshot)
  numCPUs: 2
  memoryMiB: 8192
  diskGiB: 25
  endpointIP: 192.168.1.200            # the static control-plane VIP (kube-vip)
  # vipInterface: ens192               # NIC for kube-vip; empty = auto-detect

# vSphere (CAPV) is in clusterctl's built-in provider list, but its released
# cluster template is a multi-node CPI/CSI layout. klotho needs the FLAT
# single-node template in this directory. Point fetchURL at a local clusterctl
# repository that carries provider/infrastructure/vsphere/cluster-template.yaml
# as this version's cluster template, and componentsURL at the real CAPV release
# the in-cluster operator installs:
providers:
  vsphere:
    version: v1.13.0
    # fetchURL: file:///path/to/local-capv-repo/infrastructure-vsphere/v1.13.0/infrastructure-components.yaml
    # componentsURL: https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/releases/download/v1.13.0/infrastructure-components.yaml
```

Export the password (never commit it):

```bash
export VSPHERE_PASSWORD='...'
# export VSPHERE_USERNAME='klotho@vsphere.local'   # alternative to config
```

## 2. Create

```bash
klotho create vsphere-lab1 --provider vsphere --install-kubernetes
```

Watch for: the `<cluster>-bootstrap` VM cloned and powered on in vCenter, the
VIP answering on `endpointIP:6443`, the CAPI stack coming up, and the pivot to
self-managed.

## 3. Verify adoption (the key check)

```bash
kubectl --kubeconfig <admin.kubeconfig> get vspheremachines,machines -A
```

Confirm there is **one** control-plane `VSphereMachine`/`Machine`, bound to the
`<cluster>-bootstrap` VM you can see in vCenter — **not** a second cloned
control-plane VM. If a duplicate appeared, record it in your report (this is the
unverified adoption path).

## 4. Delete

```bash
klotho delete vsphere-lab1
```

Confirm every `vsphere-lab1-*` VM is gone from vCenter and the template is
untouched. Because vCenter is reachable from the host, `Teardown` finishes the
last control-plane VM itself — no sidecar, no `--force` expected.

## Report back

Open a PR updating `docs/PROVIDERS.md` (the vSphere row's `Live-tested` and
`Tested on` columns, plus a dated note), or an issue with your vCenter/ESXi
versions and anything that needed a workaround. The three things we care about:
does it come up, does it pivot, and does `klotho delete` reach zero cleanly —
plus the adoption check in step 3.
