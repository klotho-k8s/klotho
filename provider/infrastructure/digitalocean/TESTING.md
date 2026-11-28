# Testing the DigitalOcean (CAPDO) provider

This is a recipe for exercising klotho's DigitalOcean provider against a real
DigitalOcean account. A full test is a single small control plane plus one
managed load balancer, so a short run costs a few cents of Droplet time plus a
pro-rated hour of the load balancer.

> Status: the adapter, its flat template and the whole bundle-generation path are
> covered by unit tests against a faked DigitalOcean (godo) client. The live
> create/verify/delete flow below has **not** been run end-to-end yet (no
> DigitalOcean credentials in the dev environment); the commands are the intended
> flow for the first live run. Where a step is known to need something, it says
> so.

## How CAPDO adopts node zero (the thing being tested)

klotho boots node zero itself and then has CAPDO **adopt** the running droplet
rather than create a second one. Confirmed from CAPDO's source
(`internal/controller/domachine_controller.go`, `cloud/scope/machine.go`,
`cloud/services/computes/droplets.go`, api `v1beta1`):

- CAPDO's `DOMachine` controller resolves the droplet **only** through
  `GetInstanceID()`, which parses the droplet ID out of `spec.providerID`. With no
  provider ID set, `GetInstanceID()` returns `""`, `GetDroplet("")` returns nil,
  and the controller **unconditionally calls `CreateDroplet`** — there is no
  lookup by name or tag on the create path.
- klotho's `Adopt` therefore **pre-sets** `spec.providerID: digitalocean://<id>`
  on the adopted `DOMachine` (the droplet ID klotho recorded at Launch). CAPDO
  parses that ID, `GET`s the existing droplet, adopts it, and sets the Machine's
  provider ID itself — no second Droplet is created.

So adoption rides on the pre-set `providerID`. **This is the CAPL (Linode)
model** — the opposite of CAPA/CAPG, which adopt by name/tag and must *not* see a
pre-set provider ID. The single most important thing to confirm on a live run is
that CAPDO adopts node zero and does **not** create a duplicate Droplet.

## The design, and why

- **A managed load balancer is unavoidable.** Stock CAPDO always fronts the API
  server with a managed DigitalOcean load balancer and overwrites
  `DOCluster.spec.controlPlaneEndpoint` with the load balancer's IP; it cannot be
  disabled upstream. Because klotho mints the cluster certificates and node zero's
  cloud-init *before* the node boots, it **creates the load balancer in `Prepare`**
  (named `<cluster>-apiserver`, forwarding TCP 6443, targeting the apiserver role
  tag), takes its IP as the endpoint, and injects its UUID into
  `DOCluster.spec.network.apiServerLoadbalancers.resourceId` so CAPDO **adopts**
  that load balancer rather than provisioning a second. The load balancer is a
  standing ~$12/mo charge for the life of the cluster — delete the cluster when
  done. (To reuse an existing load balancer instead, set
  `digitalocean.loadBalancerId`.)
- **Node zero is a target of that load balancer.** klotho tags node zero's droplet
  with CAPDO's apiserver role tag (`sigs-k8s-io:capdo:<cluster>:apiserver`), which
  the load balancer targets, so once node zero's API server is up the health check
  passes and the endpoint forwards to it.
- **One small control plane.** `s-2vcpu-4gb` (2 vCPU / 4 GB) is the default — the
  smallest size that clears kubeadm's two-CPU preflight. The `s-1vcpu-*` sizes are
  cheaper but fail that check.
- **A plain Ubuntu image + install-at-boot.** `ubuntu-22-04-x64` carries no
  Kubernetes, so create with `--install-kubernetes` (or
  `nodes.installKubernetes: true`), exactly like the other plain-image providers.
- **No CNI, no CCM in the bundle.** Like upstream CAPDO's own templates this one
  ships no CNI; apply one after the API server is up (below). It also bakes no
  DigitalOcean cloud-controller-manager — and therefore no DigitalOcean token —
  into the workload cluster. The token reaches only the in-cluster CAPDO
  controller, through the operator's config secret.

The template is [`cluster-template.yaml`](./cluster-template.yaml) in this
directory. It is authored in the Cluster API **v1beta2 contract** (CAPI-core
object references use `apiGroup`); the DigitalOcean infrastructure objects are
`infrastructure.cluster.x-k8s.io/v1beta1`, CAPDO's storage version.

## Where the token goes

The DigitalOcean API token comes **only from the environment**:

```sh
export DIGITALOCEAN_TOKEN=<your-digitalocean-personal-access-token>
```

It is never read from or written to the config file. klotho uses it to call the
DigitalOcean API from the host (boot node zero, create/reap the load balancer,
tear down) and hands it to the in-cluster CAPDO controller as its credential,
base64-encoded as `DO_B64ENCODED_CREDENTIALS` (which CAPDO's components decode
into `DIGITALOCEAN_ACCESS_TOKEN`). Nothing else in the workload cluster receives
it. The token needs read/write on Droplets and Load Balancers.

## The config block

```yaml
capi:
  version: v1.12.9
providers:
  digitalocean:
    version: v1.7.0
    # point this at a local clusterctl repo carrying the flat template (below)
    fetchURL: file:///ABSOLUTE/PATH/repo/infrastructure-digitalocean/v1.7.0/infrastructure-components.yaml
    # the in-cluster operator needs an http(s)/oci URL it can reach; leave the
    # real release here so it can pull the CAPDO components:
    componentsURL: https://github.com/kubernetes-sigs/cluster-api-provider-digitalocean/releases/download/v1.7.0/infrastructure-components.yaml
digitalocean:
  region: nyc3                 # a region available on your account
  size: s-2vcpu-4gb            # smallest size that passes kubeadm preflight
  image: ubuntu-22-04-x64      # plain image -> use --install-kubernetes
  sshKeys:
  - "aa:bb:cc:...:ff"          # a DO SSH key fingerprint (or numeric ID), so you
                               # can reach node zero if bootstrap fails
nodes:
  installKubernetes: true      # the image is plain Ubuntu, not a k8s image
```

`DIGITALOCEAN_TOKEN` is set in the environment, not here. Confirm the default
CAPDO release (`providers.digitalocean.version`) matches a published release; the
built-in default is a `VERIFY` value and may need bumping for a live run.

## Serving the flat template to klotho

klotho fetches a provider's components and cluster template through clusterctl.
To use this flat template instead of CAPDO's default, lay the CAPDO release out
as a local clusterctl repository and point
`providers.digitalocean.fetchURL` at it:

```
<dir>/infrastructure-digitalocean/v1.7.0/
    infrastructure-components.yaml   # from the CAPDO release, unchanged
    metadata.yaml                    # from the CAPDO release, unchanged
    cluster-template.yaml            # this directory's cluster-template.yaml
```

```sh
base=https://github.com/kubernetes-sigs/cluster-api-provider-digitalocean/releases/download/v1.7.0
mkdir -p repo/infrastructure-digitalocean/v1.7.0 && cd repo/infrastructure-digitalocean/v1.7.0
curl -sSL -O $base/infrastructure-components.yaml
curl -sSL -O $base/metadata.yaml
cp /path/to/klotho/provider/infrastructure/digitalocean/cluster-template.yaml .
```

Set `fetchURL` to a **`file://` URL** of the components file (three slashes; a
bare `C:/...` path is parsed as a URL scheme and rejected), and set
`componentsURL` to the real release so the in-cluster operator can reach it.

## The live flow (create -> verify -> delete)

```sh
export DIGITALOCEAN_TOKEN=<token>

# 1. Create. klotho creates the control plane load balancer, boots node zero
#    (s-2vcpu-4gb) with cloud-init via the Droplet user-data, installs the CAPI
#    operator + CAPDO from the release, applies the flat bundle, ADOPTS node zero
#    (sets spec.providerID: digitalocean://<id>), and pivots to self-managed. It
#    prints node zero's endpoint (the load balancer IP) and the node IP.
klotho create do-e2e --provider digitalocean --config /abs/path/config.yaml \
  --kubernetes-version v1.31.4 --control-plane-count 1 --worker-count 0

# 2. Verify. klotho writes an admin kubeconfig; check the API and the node, then
#    apply a CNI so the node goes Ready. Confirm CAPDO adopted node zero and did
#    NOT create a second Droplet.
kubectl --kubeconfig <klotho-kubeconfig> get nodes
kubectl --kubeconfig <klotho-kubeconfig> get machines,domachines -A
doctl compute droplet list --tag-name sigs-k8s-io:capdo:do-e2e   # want: one droplet
kubectl --kubeconfig <klotho-kubeconfig> apply -f <cilium or calico manifest>

# 3. Delete (deletes every droplet tagged for the cluster, then the load balancer).
klotho delete do-e2e --config /abs/path/config.yaml --watch
```

### Known caveats for a first live run

1. **Load balancer target adoption.** klotho creates the load balancer with its
   droplet target tag set to CAPDO's apiserver role tag and tags node zero with
   it. Confirm on the live run that (a) CAPDO adopts the load balancer by UUID
   without retargeting it away from node zero, and (b) the load balancer's health
   check goes healthy once node zero's API server is up. If CAPDO rewrites the
   target tag on adoption, node zero may drop out of the backend — report it.
2. **DOMachine.spec.sshKeys is required.** The template leaves it `[]`. If the
   CAPDO webhook rejects an empty list, add a real fingerprint/ID to the
   `DOMachineTemplate`s (and open an issue so the template can default one).
3. **Node providerID / CCM.** klotho's `Adopt` binds node zero's *Machine* by its
   pre-set provider ID, but the *Node* object's `spec.providerID` is set by the
   DigitalOcean cloud-controller-manager, which this cheap bundle does not ship.
   Until the DO CCM (and a CNI) are applied, nodes stay `NotReady` and only node
   zero is fully bound. This mirrors the Linode caveat.
4. **No CNI until you apply one.** The node stays `NotReady` until a CNI is
   applied (step 2), as with every CAPDO template.
5. **Multi-control-plane scaling** (`--control-plane-count` > 1) exercises CAPDO's
   *create* path for the extra machines, which has not been validated here. Start
   with a single control plane.

## Teardown and leak audit

This recipe creates Droplets and one load balancer. After a test, confirm none
are left:

```sh
# every droplet klotho created carries the tag klotho:cluster:<cluster>
doctl compute droplet list --tag-name klotho:cluster:do-e2e --format ID,Name,Status
# want: no rows

# the control plane load balancer
doctl compute load-balancer list --format ID,Name,Status | grep do-e2e-apiserver
# want: no rows
```

`klotho delete ... --watch` deletes the droplets and then the load balancer; the
commands above are the audit.

## Approximate cost

A short CP-only run is a few cents of Droplet time (one `s-2vcpu-4gb` is
~$0.036/hr) plus the load balancer, which is billed hourly (~$12/mo, so a few
cents for a 20-30 min test). The only way to spend real money is to leave the
Droplets or the load balancer running — hence the audit above.
