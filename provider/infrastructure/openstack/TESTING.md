# Testing the OpenStack (CAPO) provider

This is the recipe for exercising klotho's OpenStack provider against a real
OpenStack cloud. Unlike the cloud providers, OpenStack is **self-hostable for
free**: a single-VM [DevStack](https://docs.openstack.org/devstack/latest/) or
[MicroStack](https://canonical.com/microstack) gives you Nova, Neutron, Glance
and (optionally) Octavia to run this end to end at no cost.

> Status: the adapter, its flat template and the whole bundle-generation path are
> covered by unit tests against a faked Nova/Neutron/Glance client, and the flat
> template parses through klotho's generator into the expected objects. The live
> create/verify/delete flow below has **not** been run end-to-end yet; the
> commands are the intended flow for the first live run. Items known to need a
> live check are called out.

## How CAPO adopts node zero (the thing being tested)

klotho boots node zero itself and then has CAPO **adopt** the running Nova server
rather than create a second one. Confirmed from CAPO's source at **v0.15.2**:

- The per-machine server lives in the `OpenStackServer` reconciler
  (`controllers/openstackserver_controller.go`, `getOrCreateServer`). When it has
  no recorded `Status.InstanceID`, it calls
  `computeService.GetInstanceStatusByName(server, server.Name)` **before**
  creating one, and reuses a match ("Server already exists") instead of creating.
- `GetInstanceStatusByName` (`pkg/cloud/services/compute/instance.go`) lists Nova
  servers with an **anchored-exact** name filter (`Name: "^<name>$"`): **0**
  matches → create a new server; **1** match → adopt/reuse it; **>1** →
  `ErrMultipleMatches` with a `DuplicateServerNames` warning (no create, no
  adopt). Keep node-zero names plain (the filter is a regex).
- The Nova server **name** CAPO searches is the `OpenStackServer` object name,
  which equals the `OpenStackMachine` object name (`getOrCreateMachineServer`
  sets the `OpenStackServer` `ObjectMeta.Name` to `openStackMachine.Name`, and
  `getOrCreateServer` sets `instanceSpec.Name = openStackServer.Name`). klotho
  names node zero `<cluster>-bootstrap`, the same name `Adopt` gives the
  `OpenStackMachine` and the name the kubeadm control plane provider gives the
  bootstrap `Machine`'s `infrastructureRef`.
- klotho does **NOT** pre-set `spec.providerID`. CAPO overwrites it
  unconditionally once the server is ACTIVE
  (`openstack://<region>/<uuid>`, i.e. `openstack:///<uuid>` with no region), so a
  pre-set value is not an adoption selector. (This is the GCP/CAPD model, the
  opposite of CAPL.)

**First-live-run checks:** that CAPO's name lookup really adopts klotho's
pre-booted server (exactly one server named `<cluster>-bootstrap`, no duplicate),
and that node zero joins as a control plane member without a rollout.

## The cheap design, and why

- **No Octavia load balancer.** The flat template sets
  `spec.apiServer.managedLoadBalancer.enabled: false`. The control plane endpoint
  is a **Neutron floating IP** klotho allocates from the external network in
  `Prepare` (so the endpoint is known before cert-minting), associates with node
  zero after boot, and keeps via `spec.apiServer.floatingIP` after adoption. No
  Octavia, no standing LB cost. A single control plane has no API-server
  redundancy; set `openstack.controlPlaneEndpoint` to your own Octavia VIP for HA.
- **Bring-your-own network.** The template references the existing Neutron network
  (`openstack.network`) klotho booted node zero on, so CAPO's machines land beside
  it. klotho creates no network/subnet/router.
- **No cloud controller manager.** The kubelet is given its providerID directly
  (`openstack:///<nova-uuid>`), the only thing Cluster API needs from the cloud
  provider to bind each Machine to its Node. The OpenStack CCM (and its RBAC) is
  left out of a throwaway cluster.
- **A plain image + install-at-boot.** A stock Ubuntu cloud image carries no
  Kubernetes, so create with `--install-kubernetes` (or
  `nodes.installKubernetes: true`), like the other plain-image providers.
- **No CNI in the bundle.** Apply one after the API server is up; the node stays
  `NotReady` until you do.

The template is [`cluster-template.yaml`](./cluster-template.yaml). Its CAPI-core
objects are authored in the **v1beta2 contract** (references use `apiGroup`); the
OpenStack infrastructure objects are
`infrastructure.cluster.x-k8s.io/v1beta2`, CAPO v0.15.x's storage version (the
version upstream's own v0.15.2 template uses), so `flavor` and `image` are the
structured `{filter: {name: ...}}` form.

## Credentials

klotho authenticates to Nova/Neutron/Glance with the official
`github.com/gophercloud/gophercloud/v2` SDK, reading a **`clouds.yaml`** entry
selected by `openstack.cloud` (or `OS_CLOUD`). The same clouds.yaml is read from
`openstack.cloudsYaml` (or the standard locations: `OS_CLIENT_CONFIG_FILE`,
`./clouds.yaml`, `~/.config/openstack/clouds.yaml`, `/etc/openstack/clouds.yaml`)
and handed, **base64-encoded**, to the in-cluster CAPO controller as a per-cluster
`Secret` named `<cluster>-cloud-config` that the `OpenStackCluster.identityRef`
points at. A typical `clouds.yaml`:

```yaml
clouds:
  mycloud:
    auth:
      auth_url: https://keystone.example.com:5000/v3
      username: admin
      password: secret
      project_name: demo
      user_domain_name: Default
      project_domain_name: Default
    region_name: RegionOne
    identity_api_version: 3
```

The clouds.yaml is read from disk; it is never written back by klotho.

## Self-hosting OpenStack for free (DevStack)

A single VM (8 GB RAM, Ubuntu 22.04) is enough:

```sh
git clone https://opendev.org/openstack/devstack
cd devstack
cat > local.conf <<'EOF'
[[local|localrc]]
ADMIN_PASSWORD=secret
DATABASE_PASSWORD=$ADMIN_PASSWORD
RABBIT_PASSWORD=$ADMIN_PASSWORD
SERVICE_PASSWORD=$ADMIN_PASSWORD
# enable_plugin octavia https://opendev.org/openstack/octavia   # only if you want an LB
EOF
./stack.sh
```

Then upload a plain Ubuntu cloud image, create an external network + a tenant
network, and a keypair:

```sh
openstack image create ubuntu-22.04 --disk-format qcow2 --container-format bare \
  --file jammy-server-cloudimg-amd64.img --public
openstack keypair create --public-key ~/.ssh/id_ed25519.pub mykey
# note the external network name (DevStack's default is "public") and your
# tenant network name (default "private").
```

MicroStack (`sudo snap install microstack --beta`) is an even quicker
single-command alternative.

## The config block

```yaml
capi:
  version: v1.12.9
providers:
  openstack:
    version: v0.15.2               # VERIFY the CAPO release and CAPI-contract fit
    # point this at a local clusterctl repo carrying the flat template (below)
    fetchURL: file:///ABSOLUTE/PATH/repo/infrastructure-openstack/v0.15.2/infrastructure-components.yaml
    # the in-cluster operator needs an http(s)/oci URL it can reach; leave the
    # real release here so it can pull the CAPO components:
    componentsURL: https://github.com/kubernetes-sigs/cluster-api-provider-openstack/releases/download/v0.15.2/infrastructure-components.yaml
openstack:
  cloud: mycloud                   # the clouds.yaml entry (or set OS_CLOUD)
  # cloudsYaml: /abs/path/clouds.yaml   # or a standard location / OS_CLIENT_CONFIG_FILE
  network: private                 # the Neutron network node zero joins
  externalNetwork: public          # the public network the control plane FIP comes from
  flavor: m1.medium                # a flavor with >= 2 vCPU for kubeadm preflight
  image: ubuntu-22.04              # a plain cloud image -> install-at-boot
  sshKeyName: mykey                # so you can reach a node if bootstrap fails
  # controlPlaneEndpoint:          # leave UNSET: klotho allocates & associates a FIP
nodes:
  installKubernetes: true          # the image is plain Ubuntu, not a k8s image
```

## Serving the flat template to klotho

klotho fetches a provider's components and cluster template through clusterctl.
To use this flat template instead of CAPO's default (which provisions Octavia),
lay the CAPO release out as a local clusterctl repository and point
`providers.openstack.fetchURL` at it:

```
<dir>/infrastructure-openstack/v0.15.2/
    infrastructure-components.yaml   # from the CAPO release, unchanged
    metadata.yaml                    # from the CAPO release, unchanged
    cluster-template.yaml            # this directory's cluster-template.yaml
```

```sh
base=https://github.com/kubernetes-sigs/cluster-api-provider-openstack/releases/download/v0.15.2
mkdir -p repo/infrastructure-openstack/v0.15.2 && cd repo/infrastructure-openstack/v0.15.2
curl -sSL -O $base/infrastructure-components.yaml
curl -sSL -O $base/metadata.yaml
cp /path/to/klotho/provider/infrastructure/openstack/cluster-template.yaml .
```

Set `fetchURL` to a **`file://` URL** of the components file (three slashes; a
bare `C:/...` path is parsed as a URL scheme and rejected), and set
`componentsURL` to the real release so the in-cluster operator can reach it.

## The live flow (create -> verify -> delete)

```sh
export OS_CLOUD=mycloud

# 1. Create. klotho allocates a Neutron floating IP (the endpoint), boots node
#    zero on `openstack.flavor` with cloud-init as server user-data, associates
#    the floating IP with it, installs the CAPI operator + CAPO from the release,
#    applies the flat bundle, ADOPTS node zero (CAPO finds it by name), and pivots
#    to self-managed. It prints the endpoint.
klotho create os-e2e --provider openstack --config /abs/path/config.yaml \
  --kubernetes-version v1.33.4 --control-plane-count 1 --worker-count 0 \
  --install-kubernetes

# 2. Verify. klotho writes an admin kubeconfig; check the API and the node, then
#    apply a CNI so the node goes Ready.
kubectl --kubeconfig <klotho-kubeconfig> get nodes
kubectl --kubeconfig <klotho-kubeconfig> get machines -A
kubectl --kubeconfig <klotho-kubeconfig> apply -f <cilium or calico manifest>

# 3. Delete (CAPO tears down the servers; klotho sweeps what it owns).
klotho delete os-e2e --config /abs/path/config.yaml --watch
```

### Known caveats for a first live run

1. **CAPO release / CAPI contract fit.** `DefaultOpenStackVersion` (v0.15.2) is
   the current line; confirm it serves the CAPI v1beta2 contract klotho's core
   objects are authored in, and bump `providers.openstack.version` and the URLs
   if you pin a different release. (v0.14.x serves only `v1beta1` and would need a
   `v1beta1` template with string flavors.)
2. **providerID templating (most likely to need a tweak).** The template builds
   the kubelet `provider-id` as `openstack:///{{ ds.meta_data.uuid }}`, reading
   the Nova server UUID from the OpenStack cloud-init datasource's instance-data.
   Confirm each Node binds to its Machine (a `nodeRef` appears, `providerID`
   matches `openstack:///<uuid>`) with no CCM installed. If the key differs on
   your image's cloud-init, adjust it (candidates: `ds.meta_data.uuid`,
   `v1.instance_id`).
3. **Name adoption.** Confirm exactly one Nova server named `os-e2e-bootstrap`
   exists after CAPO reconciles (adoption, not a duplicate).
4. **Floating IP reachability.** Confirm the allocated floating IP reaches node
   zero's API server during converge (security groups must allow 6443 from
   klotho's host). The template sets
   `managedSecurityGroups.allowAllInClusterTraffic: true`; you may need an extra
   rule for the API port from outside the cluster.
5. **Region in providerID.** klotho keeps the empty-region form
   (`openstack:///<uuid>`). If your cloud requires `identityRef.region`, both the
   OpenStackCluster/Machine and the kubelet `provider-id` must carry the same
   region or the Node will not bind — out of scope for this first-cut template.

## Teardown and leak audit

Every server klotho creates carries the Nova tag
`klotho-io-cluster=<cluster>`. After a test, confirm nothing is left:

```sh
# servers
openstack server list --tags klotho-io-cluster=os-e2e        # want: none

# the allocated control plane floating IP
openstack floating ip list                                    # the one klotho made is gone

# any ports/security groups CAPO created for the cluster
openstack port list
openstack security group list
```

`klotho delete ... --watch` drives the cluster deletion, which has CAPO remove
its servers and ports, and klotho release the floating IP and any server it still
owns. **Teardown reaches zero but is not yet a hands-off finisher** — the
in-cluster CAPO controller runs on the last control-plane server it must delete,
so (like GCP/Azure/Linode) the external deletion finisher is the outstanding work.

## Approximate cost

Free on a self-hosted DevStack/MicroStack. On a paid OpenStack cloud, the only
resources are one control-plane server for the ~30 minutes it exists plus one
floating IP — no Octavia load balancer is provisioned.
