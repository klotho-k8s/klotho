# Cluster Autoscaler add-on

klotho can install the Kubernetes [Cluster Autoscaler][ca] as an add-on, both at
creation (`klotho create --autoscaler`) and day-2 (`klotho upgrade --autoscaler`;
see [docs/UPGRADES.md](UPGRADES.md)). Both paths use one shared module, so they
install byte-identical objects.

## What it does — and does not do

The autoscaler runs the native **`clusterapi`** cloud provider
(`--cloud-provider=clusterapi`). It watches the cluster's own pods and
`MachineDeployment`s and adjusts each pool's replica count between a floor and a
ceiling:

- a **pod that cannot be scheduled** for lack of room makes it grow a pool
  (up to the pool's max);
- a **node that has been under-used long enough**, whose pods fit elsewhere,
  makes it shrink a pool (down to the pool's min).

It scales the **number of nodes**, not the size of a node. It does not resize
VMs, it does not change a node's CPU/memory, and it does not touch the control
plane. Right-sizing a node is a template change, not an autoscaler job.

Because it operates on `MachineDeployment`s through the Cluster API contract, it
is **provider-agnostic**: there is no per-provider code, and the same add-on
works on every infrastructure provider klotho supports.

## How it is wired

klotho installs, in the cluster:

- a `ServiceAccount` (`cluster-autoscaler`, in `kube-system`);
- two `ClusterRole`s and their bindings — a **management** role (read/update the
  `MachineDeployment`s and their `scale` subresource, read `MachineSet`s /
  `Machine`s, and read the infrastructure `MachineTemplate`s) and a **workload**
  role (read pods/nodes and scheduling inputs, evict pods, run leader election);
- a `Deployment` running `--cloud-provider=clusterapi`, scheduled on a control
  plane (it tolerates the control-plane taint, since a self-managed cluster may
  have no workers).

This is a **self-managed single cluster**: management and workload are the same
cluster, so the autoscaler runs in-cluster and resolves both the Cluster API
objects and the Nodes through its own ServiceAccount — no second kubeconfig.
Per the upstream README, with neither `--kubeconfig` nor `--cloud-config` set it
uses the in-cluster config for the workload and falls back to it for the
management side, so `--cloud-provider=clusterapi` is the only cloud flag needed.
Discovery is scoped to this cluster with
`--node-group-auto-discovery=clusterapi:namespace=<ns>,clusterName=<name>`.

On every worker `MachineDeployment` klotho stamps the two node-group bounds (on
the object's **top-level** `metadata.annotations`):

    cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size: "1"
    cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size: "5"

The defaults are **1/5**; set them with `--autoscaler-min` / `--autoscaler-max`.
Leave the pool's `spec.replicas` free for the autoscaler to own — do not have
another controller (GitOps, a ClusterClass topology that pins replicas) also
writing that field, or they will fight.

### Image / version

The image is `registry.k8s.io/autoscaling/cluster-autoscaler`. The autoscaler is
released **per Kubernetes minor** and should match the control plane's minor, so
the tag tracks the cluster's Kubernetes version by default (e.g. a v1.33.x
cluster → `cluster-autoscaler:v1.33.x`). Override it with `--autoscaler-version`
(a published tag) or `--autoscaler-image` (a full `repo:tag`, for a mirror or
fork). klotho warns if the autoscaler's minor differs from the cluster's.

## Scale-from-zero caveat

Growing a pool from **1 → N** needs no extra information: the autoscaler reads a
running node as the template for the ones it adds.

Growing a pool from **0 → 1** is different. At zero replicas there is no sample
node, so the autoscaler must be told a would-be node's capacity. The standard
way is capacity annotations on the `MachineDeployment` (top-level
`metadata.annotations`):

    capacity.cluster-autoscaler.kubernetes.io/cpu: "4"
    capacity.cluster-autoscaler.kubernetes.io/memory: "8Gi"
    capacity.cluster-autoscaler.kubernetes.io/ephemeral-disk: "40Gi"
    capacity.cluster-autoscaler.kubernetes.io/maxPods: "110"
    # and, for GPUs: gpu-type / gpu-count; plus labels / taints

**klotho does not set these**, because the values are provider- and
flavor-specific — they describe the VM a given template clones, which klotho
cannot derive generically, and guessing wrong numbers is worse than setting
none. Instead klotho grants the autoscaler **read access to the infrastructure
`MachineTemplate`** (the `infrastructure.cluster.x-k8s.io` read rule in the
management ClusterRole), which lets the `clusterapi` provider infer the same
capacity *where the infrastructure provider publishes it in the template*.

So, concretely:

- **min ≥ 1** (the default): works out of the box; the pool is kept at ≥ 1 node
  and grows/shrinks from there.
- **min = 0** (scale-to-zero): works only if the provider publishes template
  capacity (template-inferred) **or** you add the
  `capacity.cluster-autoscaler.kubernetes.io/*` annotations to the
  `MachineDeployment` by hand with the right numbers for your flavor. klotho
  prints a finding when you ask for min 0 to make this explicit.

## Findings, never edits

Like the rest of klotho, the add-on **reports and never mutates a user's cluster
YAML to fix a problem** — the only objects it writes are its own add-on objects
and the min/max annotations it stamps. It warns (does not block) when:

- the cluster has **no worker `MachineDeployment`** to scale (it still installs,
  and idles until a pool exists);
- the autoscaler **version skews** from the cluster's Kubernetes minor;
- you request **min 0** (the scale-from-zero caveat above).

[ca]: https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/clusterapi
