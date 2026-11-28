# Kubernetes upgrades

`klotho upgrade` raises a cluster's Kubernetes version. It is pure, provider-
agnostic Cluster API orchestration: klotho never runs a second control loop and
never edits a workload. It patches the control-plane object's `spec.version`,
lets the controller roll the control-plane machines, then patches each
`MachineDeployment`. Every step is something you could do by hand with
`kubectl` through `klotho shell`; the command exists for the skew guard, the
ordering, and the rollout waits.

The version bump is identical across infrastructure providers, so there is one
upgrade command for all of them.

## Command

    klotho upgrade <cluster> --kubernetes-version v1.33.4 [flags]

The cluster may be named positionally or with `--cluster`.

| flag | effect |
| --- | --- |
| `--kubernetes-version`, `-k` | target version, e.g. `v1.33.4` (required) |
| `--control-plane-only` | upgrade only the control plane |
| `--workers-only` | upgrade only the `MachineDeployment`s |
| `--watch` | follow each rollout to completion before moving on |
| `--force` | apply the change despite skew findings |
| `--watch-timeout` | bound each watched rollout (default 20m) |
| `--control-plane-image` | point the control plane at a node image for the target version |
| `--worker-image` | point every worker pool at a node image for the target version |
| `--image-field` | infra-template field that carries the image (default `customImage`) |

With `--watch`, klotho waits for the control plane's rolling replace to finish —
the new machine joins, the old one drains and is removed, every control-plane
Machine reports the target version with a node, and none is being deleted —
*before* it touches the workers. That ordering is not optional: Cluster API
enforces a one-minor kubelet-to-apiserver skew, so the control plane must lead.
Without `--watch` klotho patches the objects and returns, leaving the
controllers to roll at their own pace.

## Skew rules

klotho checks the version change before it patches anything and reports each
violation as a finding (the same report-don't-mutate contract as `create`):

- the control plane may advance **at most one minor** per upgrade;
- the control plane may **not be downgraded**;
- a worker pool may advance at most one minor and may **not be downgraded**;
- workers may **never run ahead of the control plane** — this is what catches a
  careless `--workers-only` to a minor the control plane has not reached yet.

Findings block the upgrade. They are advice, not an error in the cluster, and
`--force` applies the change anyway. klotho never edits the cluster to resolve a
finding.

## How the target version reaches the node — the template/version finding

An upgraded Machine needs the **target version's binaries**. Where they come
from decides what `klotho upgrade` has to change, and it splits into two cases.

### 1. The image derives the version from the Machine (no template swap)

CAPD is the clean case. klotho's `DockerMachineTemplate` sets no `customImage`,
and CAPD's `DockerMachine` controller then derives the node image as
`kindest/node:v<Machine.spec.version>`. Bumping `spec.version` on the
`KubeadmControlPlane` (and each `MachineDeployment`) is therefore the *whole*
upgrade: Cluster API rolls the machines and each new one boots the matching
`kindest/node` image. This is the path the live CAPD test exercises, and
`klotho upgrade` needs no image flags for it.

The same is true for any provider whose machine image resolves the Kubernetes
version from the Machine spec at reconcile time.

### 2. The image pins the version at boot (template swap required)

klotho's **install-at-boot** path (`nodes.installKubernetes: true`, for plain
Debian/Ubuntu images, used on Proxmox) and any **prepared template** pin the
version. The install script
(`pkg/generate/install.go` → `provider/controlplane/kubeadm/scripts.go`) is
rendered **once, at create time**, with the version baked in as a literal
`WANT_VERSION`, and that rendered script is embedded as a file inside the
`KubeadmControlPlane`'s `kubeadmConfigSpec` and every `KubeadmConfigTemplate`.
It is idempotent — a node that already has the wanted kubeadm exits early — but
it **cannot read the version from the Machine's `spec.version`**: the version it
installs is fixed in the embedded file, not derived at boot. A prepared Proxmox
template is the same story: its binaries are whatever the template was built
with.

So for a version-pinned image, bumping `spec.version` alone would roll new
machines that still install the **old** version. **The template swap is the
upgrade path**: point the control plane and the workers at a machine
template/image that carries the target version, then bump `spec.version`.

`klotho upgrade` supports this directly. With `--control-plane-image` /
`--worker-image` it:

1. reads the infrastructure MachineTemplate the object references,
2. creates a **new** template (Cluster API templates are immutable once
   referenced), copied from the current one with the image field — `customImage`
   by default, overridable with `--image-field` for a provider that names it
   otherwise (a Proxmox `templateID`, say) — set to the given image,
3. repoints the object's `infrastructureRef` at the new template in the same
   patch that bumps `spec.version`.

Cluster API then rolls every machine onto the new template at the new version,
control plane first.

For Proxmox the operational shape is: build (or select) a template that carries
the target kubeadm/kubelet, then
`klotho upgrade <cluster> -k v1.34.1 --control-plane-image <templateID> --worker-image <templateID> --image-field templateID --watch`.
A fully Proxmox-native template verb is tracked in
[day-2-operations.md](day-2-operations.md).

## What klotho does not do

It does not upgrade Cluster API itself or the infrastructure provider (those are
operator `spec.version` patches, also sketched in day-2-operations.md), it does
not build template images, and it holds no opinion about anything running in the
cluster.
