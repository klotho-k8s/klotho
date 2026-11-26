# Testing the Linode (CAPL) provider cheaply

This is a recipe for exercising klotho's Linode provider against a real Linode
account without running up a bill. It uses **no NodeBalancer** (the usual
hourly-billed endpoint on Linode) and a single small control plane, so a full
test costs a few cents.

> Status: the adapter, its flat template and the whole bundle-generation path
> are covered by unit tests against a faked Linode client. The live
> create/verify/delete flow below has **not** been run end-to-end yet; the
> commands are the intended flow for the first live run. Where a step is known
> to need something, it says so.

## How CAPL adopts node zero (the thing being tested)

klotho boots node zero itself and then has CAPL **adopt** the running instance
rather than create a second one. Confirmed from CAPL's source
(`internal/controller/linodemachine_controller.go`, api `v1alpha2`):

- CAPL's `LinodeMachine` reconciler gates **every** create step — firewall, VPC,
  metadata and the instance-create itself — on `Spec.ProviderID == nil`. klotho's
  `Adopt` sets `spec.providerID: linode://<id>` on the adopted `LinodeMachine`, so
  CAPL skips all creation, parses the instance ID from the provider ID, and runs
  only its configure/boot/ready preflights against the existing instance. The
  configure step is a no-op because the template sets no `osDisk`/`dataDisks`,
  and the boot step tolerates an already-booted instance. The running node is
  left untouched.
- As a second line of defence, a Linode instance **label is unique per account**.
  klotho labels node zero `<cluster>-bootstrap` — the same name the
  `LinodeMachine` carries — so even CAPL's create path would get a 400
  "Label must be unique", look the instance up by label, and adopt it instead of
  duplicating.

So adoption rides on the pre-set `providerID`, with the matching label as a
backstop. (This is the opposite of the AWS provider, where CAPA adopts by tag
and must *not* see a pre-set provider ID.)

## The cheap design, and why

- **No NodeBalancer.** A Linode NodeBalancer is CAPL's default control plane
  endpoint and bills ~$10/mo. The template sets
  `LinodeCluster.spec.network.loadBalancerType: external`, and klotho patches
  `spec.controlPlaneEndpoint` itself, so CAPL provisions **no** NodeBalancer.
- **A control plane endpoint you supply.** klotho mints the cluster
  certificates and node zero's cloud-init *before* the node boots, and Linode
  has no free, pre-assignable floating IP, so `linode.controlPlaneEndpoint` is
  **required**. The cheapest choice is a **DNS name you control**: create an A
  record now (any placeholder), run the create, then point the record at node
  zero's public IP (klotho prints it, and it is in the Linode dashboard). kubeadm
  uses the DNS name as its `controlPlaneEndpoint` and in the API-server cert
  SANs, so it keeps working regardless of the eventual IP.
- **One small control plane.** `g6-standard-2` (2 vCPU / 4 GB) is the default —
  the smallest type that clears kubeadm's two-CPU preflight. `g6-standard-1` and
  `g6-nanode-1` are cheaper but fail that check.
- **A plain Ubuntu image + install-at-boot.** `linode/ubuntu22.04` carries no
  Kubernetes, so create with `--install-kubernetes` (or `nodes.installKubernetes:
  true`), exactly like the other plain-image providers.
- **No CNI, no CCM in the bundle.** Like upstream CAPL's own templates this one
  ships no CNI; apply one after the API server is up (below). It also sets no
  `cloud-provider=external`, so no cloud-controller-manager — and therefore no
  Linode API token — is baked into the workload cluster. The token reaches only
  the in-cluster CAPL controller, through the operator's config secret.

The template is [`cluster-template.yaml`](./cluster-template.yaml) in this
directory. It is authored in the Cluster API **v1beta2 contract** (CAPI-core
object references use `apiGroup`); the Linode infrastructure objects are
`infrastructure.cluster.x-k8s.io/v1alpha2`, which is CAPL's only API version.

## Where the token goes

The Linode API token comes **only from the environment**:

```sh
export LINODE_TOKEN=<your-linode-personal-access-token>
```

It is never read from or written to the config file. klotho uses it to call the
Linode API from the host (boot node zero, tear down) and hands it to the
in-cluster CAPL controller as its credential (nothing else in the workload
cluster receives it). The token needs read/write on Linodes (and Images/Regions/
Types read, which every token has).

## The config block

```yaml
capi:
  version: v1.12.9
providers:
  linode:
    version: v0.10.17
    # point this at a local clusterctl repo carrying the flat template (below)
    fetchURL: file:///ABSOLUTE/PATH/repo/infrastructure-linode/v0.10.17/infrastructure-components.yaml
    # the in-cluster operator needs an http(s)/oci URL it can reach; leave the
    # real release here so it can pull the CAPL components:
    componentsURL: https://github.com/linode/cluster-api-provider-linode/releases/download/v0.10.17/infrastructure-components.yaml
linode:
  region: us-ord                     # must support the Metadata service
  type: g6-standard-2                # smallest type that passes kubeadm preflight
  image: linode/ubuntu22.04          # plain image -> use --install-kubernetes
  controlPlaneEndpoint: cp.example.com  # a DNS name you control (no NodeBalancer)
  authorizedKeys:
  - ssh-ed25519 AAAA...your-key       # so you can reach a node if bootstrap fails
nodes:
  installKubernetes: true            # the image is plain Ubuntu, not a k8s image
```

`LINODE_TOKEN` is set in the environment, not here.

## Serving the flat template to klotho

klotho fetches a provider's components and cluster template through clusterctl.
To use this flat template instead of CAPL's default, lay the CAPL v0.10.17
release out as a local clusterctl repository and point `providers.linode.fetchURL`
at it:

```
<dir>/infrastructure-linode/v0.10.17/
    infrastructure-components.yaml   # from the CAPL v0.10.17 release, unchanged
    metadata.yaml                    # from the CAPL v0.10.17 release, unchanged
    cluster-template.yaml            # this directory's cluster-template.yaml
```

```sh
base=https://github.com/linode/cluster-api-provider-linode/releases/download/v0.10.17
mkdir -p repo/infrastructure-linode/v0.10.17 && cd repo/infrastructure-linode/v0.10.17
curl -sSL -O $base/infrastructure-components.yaml
curl -sSL -O $base/metadata.yaml
cp /path/to/klotho/provider/infrastructure/linode/cluster-template.yaml .
```

Set `fetchURL` to a **`file://` URL** of the components file (three slashes; a
bare `C:/...` path is parsed as a URL scheme and rejected), and set
`componentsURL` to the real release so the in-cluster operator can reach it.

## The live flow (create -> verify -> delete)

```sh
export LINODE_TOKEN=<token>

# 1. Create. klotho boots node zero (g6-standard-2) with cloud-init via the
#    Linode Metadata service, installs the CAPI operator + CAPL from the release,
#    applies the flat bundle, ADOPTS node zero (sets spec.providerID), and pivots
#    to self-managed. It prints node zero's endpoint/IP.
klotho create linode-e2e --provider linode --config /abs/path/config.yaml \
  --kubernetes-version v1.31.4 --control-plane-count 1 --worker-count 0

#    Point your DNS A record (controlPlaneEndpoint) at node zero's public IP now
#    if you did not pre-point it.

# 2. Verify. klotho writes an admin kubeconfig; check the API and the node, then
#    apply a CNI so the node goes Ready.
kubectl --kubeconfig <klotho-kubeconfig> get nodes
kubectl --kubeconfig <klotho-kubeconfig> get machines -A
kubectl --kubeconfig <klotho-kubeconfig> apply -f <cilium or calico manifest>

# 3. Delete (terminates every instance tagged for the cluster).
klotho delete linode-e2e --config /abs/path/config.yaml --watch
```

### Known caveats for a first live run

1. **DNS timing.** With `loadBalancerType: external` the endpoint is a name you
   point at node zero after it boots. If you pre-point it at a placeholder, update
   the record as soon as `klotho create` prints the node IP, or the API-server
   readiness wait will time out. A NodeBalancer (CAPL's paid default) avoids this
   but is not klotho's cheap path.
2. **No CNI until you apply one.** The node stays `NotReady` until a CNI is
   applied (step 2), as with every CAPL/CAPA template.
3. **Multi-control-plane scaling** (`--control-plane-count` > 1) exercises CAPL's
   *create* path for the extra machines, which this cheap, NodeBalancer-less
   design has not been validated against. Start with a single control plane.

## Teardown and leak audit

The only billable thing this recipe creates is the instances themselves. After a
test, confirm none are left:

```sh
# every instance klotho created carries the tag klotho.io/cluster:<cluster>
linode-cli linodes list --tags klotho.io/cluster:linode-e2e \
  --text --format 'id,label,status'          # want: no rows
```

`klotho delete ... --watch` deletes them; the command above is the audit.

## Approximate cost

A short CP-only run is a few cents: one `g6-standard-2` billed hourly
(~$0.036/hr, so ~1-2 cents for a 20-30 min test), no NodeBalancer, no extra IPs.
The only way to spend real money is to leave an instance running — hence the
audit above.
