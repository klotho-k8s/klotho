# Day-2 operations

Status: draft for discussion. Nothing here is implemented unless it says so.

## The principle

klotho creates the cluster and then gets out of the way. Once the bootstrap
node has been adopted, the cluster is an ordinary self-managed Cluster API
cluster, and every day-2 operation is a Cluster API operation: change an
object, let the controllers reconcile. klotho adds a verb only where it
holds knowledge the cluster does not have (the state record, the template
images, the operator objects it installed, the fact that one machine was
the bootstrap node), and each verb is a thin, inspectable edit of cluster
objects that a person could also make with kubectl through `klotho shell`.

That keeps the operations story honest: there is no second control loop,
nothing klotho does is hidden from `kubectl get`, and anything klotho can do
can be undone by hand.

## Operations

### 1. Upgrade Cluster API itself (core, kubeadm bootstrap, kubeadm control plane)

The Cluster API operator owns these. Each provider is a `CoreProvider`,
`BootstrapProvider` or `ControlPlaneProvider` object with `spec.version`;
raising the version makes the operator fetch the new components, run the
CRD migrations, and roll the controller deployments. Lowering it rolls
back.

Proposed verb:

    klotho upgrade <cluster> --capi v1.13.2

- Patches `spec.version` on the three objects, core first, and waits for
  each to report Ready before the next, the same wait create uses.
- Refuses a jump across more than one contract version and refuses a
  version whose contract the installed infrastructure provider does not
  support (the clusterctl metadata the generate package already reads
  carries this).
- `--fetch-url` for a fork build, as the `fork` profile does at create.

### 2. Upgrade the infrastructure provider (CAPMOX) and its dependencies

Same mechanism, `InfrastructureProvider` and the `IPAMProvider` it depends
on:

    klotho upgrade <cluster> --provider proxmox=v0.10.0

The provider's declared dependencies are upgraded with it when the new
release pins a different version, which is what `provider.Dependent`
exists for.

### 3. Change node pool sizes, add or remove pools

Pool size is `spec.replicas` on a `MachineDeployment`; the control plane
size is `spec.replicas` on the `KubeadmControlPlane`. Cluster API does the
rest: clone, join, drain, delete.

    klotho scale <cluster> --control-plane 3
    klotho scale <cluster> --pool workers=5

This is sugar over two kubectl patches. It earns a verb because it is the
most common operation, because the control plane case deserves a guard
(refuse even numbers, warn below three), and because `klotho list` should
show pool sizes next to the cluster.

Adding a pool is adding a `MachineDeployment`, a `ProxmoxMachineTemplate`
and a `KubeadmConfigTemplate`. Today that is a manifest applied through the
shell. A `klotho pool add <cluster> <name> --cores 4 --memory 8192
--replicas 2` that renders those three from the existing worker pool is a
natural later verb, but it should render and apply exactly what a person
would write, nothing more.

### 4. Recycle machines: kernel CVEs, OS patches, "just give me fresh nodes"

Cluster API machines are replaced, not patched in place. The pattern for an
OS update is:

1. Build a new template image with the fix (see 6).
2. Point the machine template at it. `ProxmoxMachineTemplate` is immutable,
   so this means creating `<name>-v2` with the new `templateID` and
   switching the `KubeadmControlPlane` or `MachineDeployment`
   `infrastructureRef` to it.
3. Cluster API rolls every machine: the control plane one at a time with a
   surge, pools according to their rolling-update strategy.

When the image is rebuilt **in place under the same template ID** (the
template build is idempotent and re-runnable), step 2 is unnecessary and a
rollout with the same spec is enough. Cluster API has a field for exactly
that, `spec.rollout.after` on both objects: set it to now and every machine
older than that timestamp is replaced with the current spec.

    klotho recycle <cluster>                  # control plane and every pool
    klotho recycle <cluster> --control-plane
    klotho recycle <cluster> --pool workers

`recycle` sets `rollout.after`. It is the day-2 verb for "the template was
rebuilt, roll everything onto it" and for "rotate every node because a
credential or a node was compromised". It never touches the image; that is
the template build's job, and the two are deliberately separate commands
so a rebuild can be reviewed before it is rolled.

Self-hosted note: the management controllers run on control plane nodes
(klotho gives them the tolerations). A control plane rollout evicts and
reschedules them; with three control planes this is a non-event, and with
one the KubeadmControlPlane surges to two before removing the old node.
Cluster API tests this configuration upstream. The one case it does not
handle, deleting the node the controllers run on during cluster deletion,
is what the self-hosted deletion feature in the fork is for, and it is
outside recycle.

### 5. Upgrade Kubernetes

A minor version upgrade is a template with the matching kubeadm and kubelet
packages plus a version bump:

    klotho upgrade <cluster> --kubernetes v1.34.1 --template 9011

- Verifies the template actually carries that kubeadm version (the prep
  writes `/etc/kubeadm-template-release`; the provider can read it through
  the guest agent before anything is changed).
- Creates the `-v2` machine templates pointing at it.
- Sets `spec.version` on the `KubeadmControlPlane` first and waits for the
  rollout, then on each `MachineDeployment`. Cluster API enforces the
  version skew, so the order is not optional and klotho should not try to
  parallelize it.

### 6. Template images

Everything above assumes a template image per Kubernetes minor and per OS
patch level. The one-time cloud-config that built template 9010 (Ubuntu
24.04, containerd, kubeadm 1.33, pre-pulled control plane images, cleaned
and sealed) is the seed of:

    klotho template build --provider proxmox --kubernetes v1.33 [--vmid 9010] [--base 9000]

It should be idempotent, dated in the template description, verified by
booting a throwaway clone and reading `/prep-complete` and the release
file, and it should be the thing a nightly job runs so a fresh image is
always available when a CVE lands. Image-builder style pipelines can
replace it later; the interface stays "a VMID that holds a prepared
template".

The prepared image is an optimisation, not a requirement. With
`nodes.installKubernetes: true` (or `klotho create --install-kubernetes`) the
generated bundle carries a script that installs containerd and the bundle's
Kubernetes version on a plain Debian or Ubuntu cloud image before kubeadm
runs, on the bootstrap node and on every machine Cluster API creates later;
`nodes.cache` can point it at an apt proxy and registry mirrors. It costs two
to three minutes per node and a working route to `pkgs.k8s.io`, and it skips
itself on a node that already has the right kubeadm, so the same bundle works
whether the template is prepared or not. Selecting templates by tag
(`proxmox.template.tags`) pairs well with it: tag the plain image on every
node once and let the script do the rest.

### 7. Certificates

Control plane certificates are one-year kubeadm certificates and Cluster
API renews them by replacing machines, driven by
`spec.rollout.before.certificatesExpiryDays` on the KubeadmControlPlane.
klotho reports the field as a finding when unset and never sets it; the
expiry date of every control plane machine is on its `KubeadmConfig` and
belongs in `klotho list --nodes`. Worker kubelet certificates rotate by
themselves.

### 8. What klotho must keep straight on its own side

- **The bootstrap node will be recycled like any other.** After the first
  control plane rollout the VM recorded in the state as the bootstrap node
  no longer exists. Teardown already works from the cluster tag and the
  pool, not from that record, so this is correct today; the state should
  simply stop claiming the node exists, and `list` should not show it as
  special.
- **VMID allocation.** klotho allocates the bootstrap VM from its configured
  range; CAPMOX allocates the rest from the Proxmox "next free ID" unless
  the machine template carries a `vmIDRange`. klotho should put its
  configured range on every generated machine template, as it does with
  the tag, so a cluster's VMs stay together. Not done yet.
- **Provider credential rotation.** With per-cluster delegated tokens
  (planned), rotation is: mint a new token, update the provider's config
  Secret, let the operator restart the provider, revoke the old token. A
  `klotho rotate <cluster>` verb, once delegation exists.
- **Recovery.** Out of scope for this document; it is its own story
  (recreate from state, etcd snapshots).

## What stays outside klotho

Anything that does not need klotho's knowledge: workload deployment,
cluster add-ons beyond what bootstrap needs, monitoring, policy. `klotho
shell` is the door to all of it, and klotho is not trying to be the tool on
the other side.

## Proposed order of work

1. `scale` (small, and the most asked-for).
2. `recycle` (one field, large payoff, needed for every CVE).
3. `template build` (moves the prep script into the tool; everything in 4
   and 5 depends on it).
4. `upgrade --capi / --provider` (operator patches plus the waits that
   already exist).
5. `upgrade --kubernetes` (needs 3, and the version skew ordering).
6. VMID range on generated templates, bootstrap record cleanup.
