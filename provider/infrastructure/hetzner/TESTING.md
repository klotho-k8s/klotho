# Testing the Hetzner Cloud (CAPH) provider

This is the recipe for exercising klotho's Hetzner provider against a real
Hetzner Cloud project. Hetzner Cloud is cheap but **not free**: the bootstrap
server and any CAPH-managed machines bill by the hour. The flat template is
designed to avoid every optional standing cost (no managed load balancer, no
private network, no floating IP), so a short control-plane-only run is cents.
Keep runs short and delete the cluster when done.

> Status: the adapter, its flat template and the whole bundle-generation path are
> covered by unit tests against a faked hcloud client, and the flat template
> parses through klotho's generator into the expected objects. The live
> create/verify/delete flow below has **not** been run end-to-end yet (there are
> no Hetzner credentials in the dev environment); the commands are the intended
> flow for the first live run. Items known to need a live check are called out.

## How CAPH adopts node zero (the thing being tested)

klotho boots node zero itself and then has CAPH **adopt** the running server
rather than create a second one. Confirmed from CAPH's source
(`pkg/scope/machine.go`, `pkg/services/hcloud/server`; api `v1beta1`):

- CAPH's `HCloudMachine` reconciler sets the server-create request's **name** to
  the `HCloudMachine`'s own name (`ServerCreateOpts.Name = scope.Name()`, and
  `Name()` returns `HCloudMachine.Name`). klotho names node zero
  `<cluster>-bootstrap`, the same name `Adopt` gives the `HCloudMachine` and the
  name the kubeadm control plane provider gives the bootstrap `Machine`'s
  `infrastructureRef`.
- A Hetzner server name is unique per project. When CAPH reconciles a machine
  whose name matches a server klotho already booted, the create returns a
  `uniqueness_error`, and CAPH's server service **resolves it by looking the
  server up by name and adopting it** ("server already exists after a uniqueness
  error, adopting it instead of failing"), then sets `providerID` itself from the
  found server. This is the same name-based adoption CAPG and CAPD use.
- klotho therefore does **NOT** pre-set `spec.providerID` on the adopted
  `HCloudMachine`. (This is the GCP/CAPD model, the opposite of CAPL.)
- The provider ID CAPH uses is **`hcloud://<numeric-server-id>`**
  (`pkg/scope/machine.go` `SetProviderID`).
- Cloud-init reaches node zero through the Hetzner server's **`user_data`** field
  — the same channel CAPH uses when it creates a server (it passes the bootstrap
  secret's value as the server `UserData`). klotho writes the identical bootstrap
  data there.

**First-live-run checks:** that CAPH's reconcile really adopts klotho's
pre-booted server by name (no duplicate appears, no "uniqueness" failure sticks),
and that node zero joins as a control plane member without a rollout.

## The cheap design, and why

- **No managed load balancer.** The flat template sets
  `controlPlaneLoadBalancer.enabled: false`. CAPH then provisions no Hetzner load
  balancer; klotho supplies the control plane endpoint (`hetzner.controlPlaneEndpoint`),
  which it patches onto the `HetznerCluster`'s `spec.controlPlaneEndpoint`. Point
  a DNS name you control at node zero's public IP after it boots (cheapest), or
  use an existing load balancer / floating IP address.
- **No private network.** `hcloudNetwork.enabled: false`; node zero and the
  CAPH-managed machines use their public IPs to reach the internet and each other.
- **cpx21 control plane.** The smallest type that clears kubeadm's two-CPU
  preflight and is available in every Hetzner location (3 shared AMD vCPU, 4 GB).
  The Intel `cx`-line equivalents are EU-only; `cax11` (Arm) also works but needs
  an arm64 image.
- **No cloud controller manager.** A `preKubeadmCommand` reads node zero's numeric
  server ID from Hetzner's instance metadata service
  (`http://169.254.169.254/hetzner/v1/metadata/instance-id`) and sets the kubelet
  `--provider-id=hcloud://<id>` directly, which is the only thing Cluster API needs
  from the cloud provider to bind each Machine to its Node. That removes the hcloud
  CCM (and its standing cost and RBAC) from a throwaway cluster.
- **A plain Ubuntu image + install-at-boot.** `ubuntu-22.04` carries no
  Kubernetes, so create with `--install-kubernetes` (or
  `nodes.installKubernetes: true`), like the other plain-image providers.
- **No CNI in the bundle.** Apply one after the API server is up; the node stays
  `NotReady` until you do.

The template is [`cluster-template.yaml`](./cluster-template.yaml). Its CAPI-core
objects are authored in the **v1beta2 contract** (references use `apiGroup`); the
Hetzner infrastructure objects are `infrastructure.cluster.x-k8s.io/v1beta1`,
CAPH's storage version.

## Credentials

CAPH's in-cluster controller reads the Hetzner API token from a **Secret named
`hetzner`** in the cluster's namespace, under the key **`hcloud`** — which is what
the flat template's `hetznerSecretRef` points at. klotho's own host-side hcloud
calls (boot node zero, tear down) authenticate with the same token from
`HCLOUD_TOKEN`. klotho also emits `HCLOUD_TOKEN` for the operator's config secret.

Create a token in the Hetzner Cloud Console (Project → Security → API Tokens,
**Read & Write**) and export it:

```sh
export HCLOUD_TOKEN=xxxxxxxx...
```

> **First-live-run check — the `hetzner` Secret.** CAPH expects the `hetzner`
> Secret to exist in the workload namespace. Confirm on the first run whether
> klotho's operator config-secret delivery creates it, or whether you must create
> it yourself before/with the bundle:
>
> ```sh
> kubectl --kubeconfig <klotho-kubeconfig> -n default \
>   create secret generic hetzner --from-literal=hcloud="$HCLOUD_TOKEN"
> ```

## An SSH key must exist in the project

CAPH's `HetznerCluster` requires at least one SSH key (and it is the only way to
reach a node to diagnose a failed bootstrap). Upload one and reference it **by
name** in `hetzner.sshKeys`:

```sh
hcloud ssh-key create --name klotho --public-key-from-file ~/.ssh/id_ed25519.pub
```

## The config block

```yaml
capi:
  version: v1.12.9
providers:
  hetzner:
    version: v1.1.13                # VERIFY the CAPH release and CAPI-contract fit
    # point this at a local clusterctl repo carrying the flat template (below)
    fetchURL: file:///ABSOLUTE/PATH/repo/infrastructure-hetzner/v1.1.13/infrastructure-components.yaml
    # the in-cluster operator needs an http(s)/oci URL it can reach; leave the
    # real release here so it can pull the CAPH components:
    componentsURL: https://github.com/syself/cluster-api-provider-hetzner/releases/download/v1.1.13/infrastructure-components.yaml
hetzner:
  location: fsn1                    # fsn1 | nbg1 | hel1 | ash | hil | sin
  serverType: cpx21                 # smallest type that passes kubeadm preflight
  # image: defaults to ubuntu-22.04 (plain -> install-at-boot)
  sshKeys:
  - klotho                          # a key already uploaded to the project (by name)
  controlPlaneEndpoint: cp.example.com   # DNS name you point at node zero after boot
nodes:
  installKubernetes: true           # the image is plain Ubuntu, not a k8s image
```

The token is **not** in the file; it comes from `HCLOUD_TOKEN`.

## Serving the flat template to klotho

klotho fetches a provider's components and cluster template through clusterctl.
To use this flat template instead of CAPH's default, lay the CAPH release out as
a local clusterctl repository and point `providers.hetzner.fetchURL` at it:

```
<dir>/infrastructure-hetzner/v1.1.13/
    infrastructure-components.yaml   # from the CAPH release, unchanged
    metadata.yaml                    # from the CAPH release, unchanged
    cluster-template.yaml            # this directory's cluster-template.yaml
```

```sh
base=https://github.com/syself/cluster-api-provider-hetzner/releases/download/v1.1.13
mkdir -p repo/infrastructure-hetzner/v1.1.13 && cd repo/infrastructure-hetzner/v1.1.13
curl -sSL -O $base/infrastructure-components.yaml
curl -sSL -O $base/metadata.yaml
cp /path/to/klotho/provider/infrastructure/hetzner/cluster-template.yaml .
```

Set `fetchURL` to a **`file://` URL** of the components file (three slashes; a
bare `C:/...` path is parsed as a URL scheme and rejected), and set
`componentsURL` to the real release so the in-cluster operator can reach it.

## The live flow (create -> verify -> delete)

```sh
export HCLOUD_TOKEN=xxxxxxxx...

# 1. Create. klotho boots node zero (cpx21) with cloud-init via the server's
#    user_data, installs the CAPI operator + CAPH from the release, applies the
#    flat bundle, ADOPTS node zero (CAPH finds it by name), and pivots to
#    self-managed. It prints the endpoint. Point your controlPlaneEndpoint DNS
#    record at node zero's public IP once it is up.
klotho create hetzner-e2e --provider hetzner --config /abs/path/config.yaml \
  --kubernetes-version v1.33.4 --control-plane-count 1 --worker-count 0 \
  --install-kubernetes

# 2. Verify. klotho writes an admin kubeconfig; check the API and the node, then
#    apply a CNI so the node goes Ready.
kubectl --kubeconfig <klotho-kubeconfig> get nodes
kubectl --kubeconfig <klotho-kubeconfig> get machines -A
kubectl --kubeconfig <klotho-kubeconfig> apply -f <cilium or calico manifest>

# 3. Delete (CAPH tears down the servers; klotho sweeps what it owns by label).
klotho delete hetzner-e2e --config /abs/path/config.yaml --watch
```

### Known caveats for a first live run

1. **CAPH release / CAPI contract fit.** `DefaultHetznerVersion` (v1.1.13) is a
   placeholder. Confirm the CAPH release you use installs under the CAPI v1.12.x
   core klotho installs and that its CRDs accept the flat template's fields; bump
   `providers.hetzner.version` and the URLs if not.
2. **The `hetzner` Secret.** See "Credentials" — confirm whether klotho delivers
   it or you must create it.
3. **Name adoption.** Confirm CAPH adopts the `<cluster>-bootstrap` server klotho
   booted (one server, not two) and that node zero joins without a rollout.
4. **providerID binding.** The template sets the kubelet `--provider-id` to
   `hcloud://<server-id>` from Hetzner metadata. Confirm each Node binds to its
   Machine (a `nodeRef` appears) with no CCM installed, and that the metadata URL
   is reachable from the server during bootstrap.
5. **Endpoint reachability.** `hetzner.controlPlaneEndpoint` must resolve to node
   zero (or a load balancer fronting it) by the time the converge phase dials the
   API server; node zero's own public IP is always in the apiserver cert SANs.

## Teardown and leak audit

Everything klotho creates carries the label `klotho.io/cluster=<cluster>`. After
a test, confirm nothing is left:

```sh
# servers (want: none)
hcloud server list --selector "klotho.io/cluster=hetzner-e2e"

# any load balancer / network / floating IP (the flat template creates none, but
# audit in case a field was changed)
hcloud load-balancer list
hcloud network list
hcloud floating-ip list
```

`klotho delete ... --watch` drives the cluster deletion, which has CAPH remove
its own servers, and klotho sweeps any server it still owns by label. The hcloud
commands above are the audit.

## Teardown status

Like GCP, Azure and Linode today, klotho's `Teardown` **reaches zero** (deletes
every server carrying the cluster label, plus anything recorded in state) but is
**not yet the hands-off external finisher**: the self-hosted deletion deadlock
(the CAPH controller runs on the last control-plane server it must delete) is
solved by the separate, shared, not-yet-built delete finisher. Until then, a
`klotho delete --watch` drives the deletion from the host and the label audit
above confirms zero.

## Approximate cost

A short control-plane-only run is cents: one `cpx21` billed per hour (~€0.007/h)
for the ~30 minutes it exists, no load balancer, no network, no floating IP. The
only way to spend real money is to leave the cluster running, hence the audit
above.
