# Testing the GCP (CAPG) provider

This is the recipe for exercising klotho's GCP provider against a real Google
Cloud project. Unlike the Linode and AWS recipes, a GCP cluster is **not** free
of standing cost: stock CAPG always provisions a managed external load balancer
for the API server, and that cannot be disabled upstream (see "Cost", below).
Keep runs short and delete the cluster when done.

> Status: the adapter, its flat template and the whole bundle-generation path
> are covered by unit tests against a faked Compute client, and the flat template
> parses through klotho's generator into the expected objects. The live
> create/verify/delete flow below has **not** been run end-to-end yet; the
> commands are the intended flow for the first live run. Items known to need a
> live check are called out.

## How CAPG adopts node zero (the thing being tested)

klotho boots node zero itself and then has CAPG **adopt** the running instance
rather than create a second one. Confirmed from CAPG's source
(`cloud/services/compute/instances/reconcile.go`, `createOrGetInstance`; api
`v1beta1`):

- CAPG's `GCPMachine` reconciler looks the instance up with
  `Instances.Get(project, zone, name)` **before** creating one, and only calls
  `Insert` when `Get` returns a 404. GCE instance names are unique per
  (project, zone), so an instance klotho already booted under that name is found
  and reused — no duplicate, no "already exists" error. This is the same
  name-based adoption CAPD and CAPMOX use.
- The instance **name** CAPG expects is the `GCPMachine`'s own name, verbatim
  (`cloud/scope/machine.go`, `InstanceSpec` sets `Name: m.Name()` where
  `m.Name()` returns `GCPMachine.Name`). klotho names node zero
  `<cluster>-bootstrap`, the same name `Adopt` gives the `GCPMachine` and the
  name the kubeadm control plane provider gives the bootstrap `Machine`'s
  `infrastructureRef`.
- klotho does **NOT** pre-set `spec.providerID` on the adopted `GCPMachine`.
  CAPG evaluates a pre-set providerID only on the *NotFound* branch, where it
  would mark the machine permanently failed. On the adopt path (instance found)
  CAPG sets the providerID itself from the instance. (This is the AWS/CAPD model,
  the opposite of CAPL.)
- Cloud-init reaches node zero through the GCE metadata key **`user-data`** —
  the exact key CAPG's instance service uses (`const metadataKeyUserData =
  "user-data"`). klotho writes the bootstrap data to the same key, so the adopted
  instance used the identical cloud-init path.

**First-live-run checks:** that CAPG's `Get` really adopts klotho's pre-booted
instance (no duplicate appears), and that node zero joins as a control plane
member without a rollout.

## Cost: the managed load balancer is unavoidable

Stock CAPG **always** reconciles an external load balancer for the API server —
a global forwarding rule, backend service, health check and a **global static
IP** — with no way to disable it (`controllers/gcpcluster_controller.go` wires
the `loadbalancers` reconciler unconditionally; the `LoadBalancerType` enum has
no `None`). It also **overwrites** `GCPCluster.spec.controlPlaneEndpoint` with
the load balancer's address.

klotho works *with* this rather than against it. Because klotho must know the
endpoint before it mints the certificates and node zero's cloud-init, `Prepare`
**reserves the very global IP CAPG's load balancer will use** — a global
external address named `<cluster>-apiserver`, which is exactly the name CAPG's
`ClusterScope.AddressSpec` derives (`<cluster>-<role>`, role `apiserver`). CAPG's
`createOrGetAddress` is get-or-create by that name, so it **adopts** the reserved
address instead of allocating a new one, and the endpoint CAPG writes back equals
the one klotho baked into the certificates. So klotho adopts both the instance
(by name) and the LB IP (by name).

Rough cost: a global forwarding rule + a standing load balancer is on the order
of **~$20-30/month even idle**, plus the reserved IP and any egress. A short
CP-only test is a few dollars at most; the real risk is leaving the cluster (and
its load balancer) running. **Delete when done** and run the leak audit below.

> If a no-load-balancer / public-IP-only control plane is ever wanted, it needs a
> CAPG fork (add a `LoadBalancerType: None` that skips the loadbalancers
> reconciler and honours a pre-set `controlPlaneEndpoint`) — consistent with
> klotho's forks-only approach. The adapter is written so that supplying
> `gcp.controlPlaneEndpoint` already bypasses klotho's address reservation, which
> is the hook such a fork would use.

## The cheap design, and why

- **Preemptible (Spot) machines.** The flat template sets `preemptible: true` on
  every CAPG-managed machine. Node zero itself is launched on-demand by default
  (so a reclaim mid-bootstrap does not strand the create); set `gcp.preemptible:
  true` to make node zero Spot too.
- **e2-medium control plane.** The smallest sane type that still clears kubeadm's
  two-CPU preflight. `e2-small`/`e2-micro` are cheaper but tight on memory.
- **Public IP, no Cloud NAT.** Every node gets an ephemeral external IP
  (`publicIP: true`), so it reaches the internet to pull images with no Cloud NAT
  gateway to pay for.
- **No cloud controller manager.** The kubelet is given its providerID directly
  (`gce://<project>/<zone>/<hostname>`), which is the only thing Cluster API needs
  from the cloud provider to bind each Machine to its Node. That removes the GCP
  CCM (and its standing cost and RBAC) from a throwaway cluster. The in-tree `gce`
  provider is deliberately avoided — it is removed from modern Kubernetes.
- **A plain Ubuntu image + install-at-boot.** `ubuntu-2204-lts` (ubuntu-os-cloud)
  carries no Kubernetes, so create with `--install-kubernetes` (or
  `nodes.installKubernetes: true`), like the other plain-image providers.
- **No CNI in the bundle.** Apply one after the API server is up; the node stays
  `NotReady` until you do.

The template is [`cluster-template.yaml`](./cluster-template.yaml). Its CAPI-core
objects are authored in the **v1beta2 contract** (references use `apiGroup`); the
GCP infrastructure objects are `infrastructure.cluster.x-k8s.io/v1beta1`, CAPG's
storage version.

## Credentials

CAPG's in-cluster controller expects a service-account key as
**`GCP_B64ENCODED_CREDENTIALS`** (base64 of the SA JSON) — klotho emits exactly
that for the operator's config secret from `gcp.credentialsFile`. klotho's own
host-side Compute calls (boot node zero, reserve the IP, tear down) authenticate
with the same file, or with Application Default Credentials when it is unset.

Create a service account with roles **Compute Admin** (`roles/compute.admin`) and
**Service Account User** (`roles/iam.serviceAccountUser`), download its JSON key,
and either point `gcp.credentialsFile` at it or set
`GOOGLE_APPLICATION_CREDENTIALS`:

```sh
gcloud iam service-accounts create klotho-capg --project "$PROJECT"
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member "serviceAccount:klotho-capg@$PROJECT.iam.gserviceaccount.com" \
  --role roles/compute.admin
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member "serviceAccount:klotho-capg@$PROJECT.iam.gserviceaccount.com" \
  --role roles/iam.serviceAccountUser
gcloud iam service-accounts keys create gcp-sa.json \
  --iam-account "klotho-capg@$PROJECT.iam.gserviceaccount.com"
export GOOGLE_APPLICATION_CREDENTIALS="$PWD/gcp-sa.json"
```

The key is read from disk; it is never written back by klotho.

## The config block

```yaml
capi:
  version: v1.12.9
providers:
  gcp:
    version: v1.8.1                 # VERIFY the CAPG release and CAPI-contract fit
    # point this at a local clusterctl repo carrying the flat template (below)
    fetchURL: file:///ABSOLUTE/PATH/repo/infrastructure-gcp/v1.8.1/infrastructure-components.yaml
    # the in-cluster operator needs an http(s)/oci URL it can reach; leave the
    # real release here so it can pull the CAPG components:
    componentsURL: https://github.com/kubernetes-sigs/cluster-api-provider-gcp/releases/download/v1.8.1/infrastructure-components.yaml
gcp:
  project: my-gcp-project
  region: us-central1
  zone: us-central1-a
  machineType: e2-medium           # smallest type that passes kubeadm preflight
  # image: defaults to ubuntu-os-cloud / ubuntu-2204-lts (plain -> install-at-boot)
  network: default                 # the project's auto-mode VPC
  # subnet: default
  credentialsFile: /abs/path/gcp-sa.json   # or set GOOGLE_APPLICATION_CREDENTIALS
  authorizedKeys:
  - ssh-ed25519 AAAA...your-key     # so you can reach a node if bootstrap fails
  # controlPlaneEndpoint:           # leave UNSET: klotho reserves the LB's IP
nodes:
  installKubernetes: true           # the image is plain Ubuntu, not a k8s image
```

## Serving the flat template to klotho

klotho fetches a provider's components and cluster template through clusterctl.
To use this flat template instead of CAPG's default, lay the CAPG release out as
a local clusterctl repository and point `providers.gcp.fetchURL` at it:

```
<dir>/infrastructure-gcp/v1.8.1/
    infrastructure-components.yaml   # from the CAPG release, unchanged
    metadata.yaml                    # from the CAPG release, unchanged
    cluster-template.yaml            # this directory's cluster-template.yaml
```

```sh
base=https://github.com/kubernetes-sigs/cluster-api-provider-gcp/releases/download/v1.8.1
mkdir -p repo/infrastructure-gcp/v1.8.1 && cd repo/infrastructure-gcp/v1.8.1
curl -sSL -O $base/infrastructure-components.yaml
curl -sSL -O $base/metadata.yaml
cp /path/to/klotho/provider/infrastructure/gcp/cluster-template.yaml .
```

Set `fetchURL` to a **`file://` URL** of the components file (three slashes; a
bare `C:/...` path is parsed as a URL scheme and rejected), and set
`componentsURL` to the real release so the in-cluster operator can reach it.

## The live flow (create -> verify -> delete)

```sh
export GOOGLE_APPLICATION_CREDENTIALS=/abs/path/gcp-sa.json

# 1. Create. klotho reserves the global IP CAPG's LB will use, boots node zero
#    (e2-medium) with cloud-init via GCE metadata "user-data", installs the CAPI
#    operator + CAPG from the release, applies the flat bundle, ADOPTS node zero
#    (CAPG finds it by name), and pivots to self-managed. It prints the endpoint.
klotho create gcp-e2e --provider gcp --config /abs/path/config.yaml \
  --kubernetes-version v1.33.4 --control-plane-count 1 --worker-count 0 \
  --install-kubernetes

# 2. Verify. klotho writes an admin kubeconfig; check the API and the node, then
#    apply a CNI so the node goes Ready.
kubectl --kubeconfig <klotho-kubeconfig> get nodes
kubectl --kubeconfig <klotho-kubeconfig> get machines -A
kubectl --kubeconfig <klotho-kubeconfig> apply -f <cilium or calico manifest>

# 3. Delete (CAPG tears down the LB and instances; klotho sweeps what it owns).
klotho delete gcp-e2e --config /abs/path/config.yaml --watch
```

### Known caveats for a first live run

1. **CAPG release / CAPI contract fit.** `DefaultGCPVersion` (v1.8.1) is a
   placeholder. Confirm the CAPG release you use supports the CAPI v1beta2
   contract klotho's core objects are authored in; bump `providers.gcp.version`
   and the URLs if not.
2. **Address adoption.** Confirm CAPG's load balancer adopts the
   `<cluster>-apiserver` global address klotho reserved (one address, not two) and
   that the endpoint stays the reserved IP.
3. **providerID binding.** The template sets the kubelet `provider-id` to
   `gce://<project>/<zone>/<hostname>`. Confirm each Node binds to its Machine
   (a `nodeRef` appears) with no CCM installed.
4. **Standing load-balancer cost.** See "Cost" above — do not leave the cluster
   running.

## Teardown and leak audit

Everything klotho creates carries the label `klotho-io-cluster=<cluster>` (GCP
label keys cannot hold dots or slashes). After a test, confirm nothing is left:

```sh
# instances
gcloud compute instances list \
  --filter="labels.klotho-io-cluster=gcp-e2e" --project "$PROJECT"   # want: none

# the reserved control plane IP
gcloud compute addresses list --global \
  --filter="name=gcp-e2e-apiserver" --project "$PROJECT"             # want: none

# the load balancer CAPG created (forwarding rule + static IP + backend)
gcloud compute forwarding-rules list --global --project "$PROJECT"
gcloud compute addresses list --global --project "$PROJECT"
gcloud compute target-tcp-proxies list --project "$PROJECT"
gcloud compute backend-services list --global --project "$PROJECT"
```

`klotho delete ... --watch` drives the cluster deletion, which has CAPG remove
its own load balancer, forwarding rule, backend service and health check, and
klotho release the reserved address and any instance it still owns. The commands
above are the audit — the standing load balancer is the thing that bills, so make
sure it is gone.

## Approximate cost

A short CP-only run is a few dollars: one preemptible/e2-medium instance billed
per second, plus the control-plane load balancer and its static IP for the ~30
minutes it exists. The only way to spend real money is to leave the cluster — and
therefore its load balancer — running, hence the audit above.
