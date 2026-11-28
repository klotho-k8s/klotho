# Testing the OCI (CAPOCI) provider

This is the recipe for exercising klotho's Oracle Cloud Infrastructure provider
against a real OCI tenancy. The **OCI Always Free tier** (two `VM.Standard.A1.Flex`
Ampere instances, a free VCN, and no load balancer if you skip LB management) or
a trial-credit account is enough to run it end-to-end at little or no cost. Keep
runs short and delete the cluster when done.

> Status: the adapter, its flat template and the whole bundle-generation path are
> covered by unit tests against a faked OCI SDK client, and the flat template
> parses through klotho's generator into the expected objects. The live
> create → pivot → delete flow below has **not** been run end-to-end yet; the
> commands are the intended flow for the first live run. Items known to need a
> live check are called out.

## How CAPOCI adopts node zero (the thing being tested)

klotho boots node zero itself and then has CAPOCI **adopt** the running instance
rather than launch a second one. Confirmed from CAPOCI's source
(`cloud/scope/machine.go`, `cloud/ociutil/ociutil.go`; api `v1beta2`):

- CAPOCI's `OCIMachine` reconciler resolves a machine's instance with
  `GetMachine` → `GetMachineByDisplayName`, which calls
  `core.ListInstances(compartmentId, displayName=OCIMachine.Name)` and accepts a
  returned instance only when `IsResourceCreatedByClusterAPI(instance.FreeformTags)`
  is true. So adoption is **by display name, cross-checked by freeform tags**.
- `IsResourceCreatedByClusterAPI` rebuilds `BuildClusterTags(ociResourceIdentifier)`
  = `{CreatedBy: OCIClusterAPIProvider, ClusterResourceIdentifier: <id>}` and
  requires both to be present on the instance. klotho therefore stamps both tags
  on node zero at launch, with `ClusterResourceIdentifier` equal to the value the
  flat template pins into `OCICluster.spec.ociResourceIdentifier` (`${CLUSTER_NAME}`).
  It also stamps its own `klotho.io/cluster=<cluster>` ownership tag for teardown.
- The **display name** CAPOCI expects is the `OCIMachine`'s own name. klotho names
  node zero `<cluster>-bootstrap`, the same name `Adopt` gives the `OCIMachine`
  and the name the kubeadm control plane provider gives the bootstrap `Machine`'s
  `infrastructureRef`.
- klotho does **NOT** pre-set `spec.providerID` on the adopted `OCIMachine`.
  CAPOCI's lookup keys on display name + tags and sets the provider ID itself from
  the found instance (`machine.Spec.ProviderID = oci://<instance-ocid>`). This is
  the GCP/CAPD model, the opposite of Linode/CAPL.
- Cloud-init reaches node zero through the instance metadata key **`user_data`**
  (base64) — the exact key CAPOCI uses (`metadata["user_data"] =
  base64(cloudInitData)`). klotho writes the bootstrap data to the same key.

**First-live-run checks:** that CAPOCI's display-name + tag lookup really adopts
klotho's pre-booted instance (no duplicate appears in the compartment), and that
node zero joins as a control plane member without a rollout.

## Endpoint / load balancer

Stock CAPOCI always fronts the API server with a managed OCI load balancer (a
Network Load Balancer by default) and populates `OCICluster.spec.controlPlaneEndpoint`
from it. klotho mints the certificates and node zero's cloud-init (which carry
the endpoint) **before** the node boots, so the endpoint must be known up front.
The flat template therefore:

- sets the annotation `cluster.x-k8s.io/skip-apiserver-lb-management: "true"` on
  the `Cluster` (CAPOCI's `skipApiserverManagement` honours it and manages no LB),
  and
- sets `OCICluster.spec.controlPlaneEndpoint` to the address klotho minted the
  certificates for (`oci.controlPlaneEndpoint`).

For a **single node**, point `oci.controlPlaneEndpoint` at node zero's own public
IP (reserve one, or use the ephemeral public IP the public subnet assigns and set
the endpoint to it after Launch — note the ephemeral-IP case needs the endpoint
known before cert-minting, so a **reserved** public IP is the clean path). For
**HA**, point it at an OCI NLB you manage yourself.

**First-live-run check:** that the skip-LB annotation is honoured (no NLB is
created) and the API server is reachable at the supplied endpoint with the minted
certificate (the node's own IP is in the cert SANs).

## Networking: existing VCN, adopted in place

klotho boots node zero into an **existing** subnet (`oci.subnetId`) in an existing
VCN (`oci.vcnId`), and the flat template points CAPOCI at the same ones
(`networkSpec.vcn.skip: true`, each gateway `skip: true`, each subnet with its
`id` and `skip: true`). CAPOCI then creates no VCN and adopts node zero in place.
Bring a **public** subnet with a route to an internet gateway so the node can pull
images (the free tier's default VCN wizard creates exactly this).

**First-live-run check:** that CAPOCI tolerates the four subnet roles pointing at
one subnet, and does not try to reconcile gateways it was told to skip.

## Prerequisites

- An OCI tenancy and a **compartment** OCID (`oci.compartmentId`).
- A **region** (e.g. `us-ashburn-1`) and an **availability domain** name
  (e.g. `Uocm:US-ASHBURN-AD-1` — `oci iam availability-domain list`).
- An existing **VCN** OCID and a **public subnet** OCID in it.
- A plain **Ubuntu image** OCID for the region and architecture
  (`oci compute image list --compartment-id <c> --operating-system "Canonical Ubuntu"`).
  For the Always Free Ampere shape use an **arm64** Ubuntu image and
  `oci.shape: VM.Standard.A1.Flex`.
- Host auth: an OCI SDK **config file** at `~/.oci/config` (`oci setup config`), or
  run klotho on an OCI instance in a dynamic group and set
  `oci.useInstancePrincipal: true`.
- The in-cluster CAPOCI controller needs its own credential after pivot: either an
  `OCIClusterIdentity` referencing a secret, or (recommended for self-managed) node
  zero in a **dynamic group** with a policy allowing it to manage compute/network
  in the compartment. Set this up out of band; klotho hands over no credential.

## Configure

`~/.config/klotho/config.yaml` (or a profile):

```yaml
oci:
  compartmentId: ocid1.compartment.oc1..aaaa...
  region: us-ashburn-1
  availabilityDomain: "Uocm:US-ASHBURN-AD-1"
  vcnId: ocid1.vcn.oc1.iad.aaaa...
  subnetId: ocid1.subnet.oc1.iad.aaaa...
  imageId: ocid1.image.oc1.iad.aaaa...        # plain Ubuntu (arm64 for A1.Flex)
  shape: VM.Standard.A1.Flex                   # Always Free Ampere; or VM.Standard.E4.Flex
  ocpus: 2
  memoryGBs: 12
  assignPublicIp: true
  controlPlaneEndpoint: 203.0.113.10           # a reserved public IP you control
  sshAuthorizedKeys:
    - ssh-ed25519 AAAA... you@host
  # configFile: ~/.oci/config                  # optional; default is ~/.oci/config
  # profile: DEFAULT
  # useInstancePrincipal: true                 # instead of a config file
```

## Create → verify → pivot

```sh
# 1. Create: klotho boots node zero (OCI compute instance, user_data cloud-init),
#    installs the operator + CAPOCI, applies the flat bundle, and CAPOCI adopts
#    the running instance by display name + freeform tags.
klotho create oci1 --provider oci --control-plane kubeadm --install-kubernetes

# 2. Watch the compartment: exactly ONE instance named "oci1-bootstrap" should
#    exist (no duplicate from CAPOCI).
oci compute instance list --compartment-id <compartmentId> \
  --query "data[?\"display-name\"=='oci1-bootstrap'].id"

# 3. Pull the admin kubeconfig and confirm the API server answers at the endpoint.
klotho credential oci1 > /tmp/oci1.kubeconfig
KUBECONFIG=/tmp/oci1.kubeconfig kubectl get nodes

# 4. Apply a CNI (none is shipped), then confirm the node goes Ready and the
#    CAPI stack reports the cluster Provisioned / the machine Running.
KUBECONFIG=/tmp/oci1.kubeconfig kubectl apply -f <cilium-or-calico>.yaml
KUBECONFIG=/tmp/oci1.kubeconfig kubectl get machines,ocimachines -A
```

Things to confirm on the first live run:
1. Node zero comes up and the CAPI stack reports Ready after pivot.
2. CAPOCI **adopted** node zero (its `OCIMachine` has `spec.providerID:
   oci://<ocid>` matching the booted instance; no second instance was launched).
3. No NLB was created (skip-LB annotation honoured) and the endpoint is reachable.

## Delete

```sh
# Reaches zero by terminating every instance carrying klotho's ownership freeform
# tag. Like GCP/Linode, this is NOT yet hands-off: the in-cluster CAPOCI
# controller runs on the last control-plane node, so the final terminate needs
# --force.
klotho delete oci1 --force
```

After delete, confirm nothing is left:

```sh
oci compute instance list --compartment-id <compartmentId> \
  --query "data[?\"freeform-tags\".\"klotho.io/cluster\"=='oci1']"
```

**Teardown finisher (TODO):** the external-API deletion sidecar that finishes the
last node after klotho exits is not built for OCI yet. Until it is, check by hand
that these are gone after a delete: the bootstrap/control-plane instances, any
block volumes not set to preserve, and — if you did NOT skip LB management or let
CAPOCI manage the VCN — the NLB, VCN, subnets and the dynamic-group policy you
created for the controller.

## Report back

Please update `docs/PROVIDERS.md` (the OCI row's `Live-tested` / `Tested on`
columns and a dated note) and open a PR or issue with your region, shape, image,
CAPOCI version, and anything that needed a workaround — especially whether the
display-name + freeform-tag adoption worked without a duplicate instance.
