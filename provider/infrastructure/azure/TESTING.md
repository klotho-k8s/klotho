# Testing the Azure (CAPZ) provider cheaply

This is a recipe for exercising klotho's Azure provider against a real Azure
subscription without running up a bill. It documents the design, the
service-principal setup, and the live create/verify/delete/audit flow with exact
commands.

> Status: this provider was built and unit-tested offline (faked Azure client);
> it has **not** been run live yet. No Azure credentials were present on the
> build machine. Everything below the "What was validated" section is the
> intended flow for a live tester, including the one Azure cost that cannot be
> avoided (below) and the network details that most need live validation.

## The adoption model (confirmed from CAPZ source)

klotho boots node zero itself (so the control plane endpoint is known before the
certificates are minted) and CAPZ **adopts** it rather than creating a second
VM. Confirmed against CAPZ `v1.23.x` source:

- CAPZ's `virtualmachines.Service.Reconcile` **Gets** the VM by
  `(node resource group, resource name)` before any create. The resource name
  is `MachineScope.Name()`: the VM name parsed from `AzureMachine.spec.providerID`
  when it is set, otherwise the `AzureMachine`'s own name
  (`azure/scope/machine.go`).
- When that Get finds a VM that is not `Failed`, `VMSpec.Parameters(existing)`
  returns `(nil, nil)` — Azure's "no change" — so CAPZ issues **no**
  CreateOrUpdate and leaves the running VM untouched; it only sets the provider
  ID from the real resource ID and records the addresses and state
  (`azure/services/virtualmachines/virtualmachines.go`, `spec.go`).
- A pre-set provider ID does **not** break adoption (the opposite of CAPA): CAPZ
  uses it to locate the VM, and refuses to recreate a VM whose provider ID is set
  but whose VM is missing ("do not recreate it as Machines are immutable").

So klotho creates the VM named `<cluster>-bootstrap` in the cluster resource
group, and `Adopt` renders an `AzureMachine` named `<cluster>-bootstrap` with
`spec.providerID` set to the VM's resource ID. Either the name or the provider ID
alone would adopt the same VM; klotho sets both.

## Auth: a service principal (no workload identity, no ASO)

CAPZ's webhook **requires** `AzureCluster.spec.identityRef`
(`internal/webhooks/azurecluster_validation.go`: "identityRef is required"), so
the flat template includes an `AzureClusterIdentity` of type `ServicePrincipal`
and a `Secret` holding the client secret. This is the simplest fully-supported
model — no workload identity, no Azure Service Operator.

Create a service principal with Contributor on the subscription (or on the
resource group), then put its fields in config / the environment:

```sh
az ad sp create-for-rbac --name klotho-capz \
  --role Contributor \
  --scopes /subscriptions/<SUBSCRIPTION_ID>
# -> appId (clientId), password (clientSecret), tenant (tenantId)
```

klotho:
- hands `AZURE_SUBSCRIPTION_ID` / `AZURE_TENANT_ID` / `AZURE_CLIENT_ID` /
  `AZURE_CLIENT_SECRET` to the CAPI operator as the CAPZ manager's configSecret
  (`providerSecretData`), and
- renders the per-cluster `AzureClusterIdentity` + `Secret` into the bundle (the
  base64 client secret is a generation-time variable only; create.go does not
  persist it into klotho's state record).

## The cheap design, and the one cost CAPZ cannot drop

A default CAPZ cluster creates a managed vnet and a **public** Standard API
server load balancer (with a public IP). This recipe removes what it can:

- **Bring-your-own network.** klotho requires an existing resource group,
  virtual network and subnet (`azure.resourceGroup/vnet/subnet`); it creates no
  network, so nothing it does not own can leak. The VM, NIC and public IP it
  creates all go in that resource group, tagged `klotho.io/cluster=<cluster>`.
- **Node zero's own static public IP is the endpoint.** klotho allocates one
  static Standard public IP in `Prepare`, attaches it to node zero's NIC in
  `Launch`, and sets it as `AzureCluster.spec.controlPlaneEndpoint`. CAPZ only
  fills the endpoint when it is empty, so it keeps klotho's value and the API is
  served directly on node zero — no public load balancer IP is used for the API.
- **`apiServerLB.type: Internal`.** This is the unavoidable part: CAPZ **always**
  reconciles an API server load balancer (there is no `disabled`, unlike CAPA's
  `loadBalancerType: disabled` or CAPL's `external`). `Internal` keeps it on a
  private frontend IP and allocates **no extra public IP**. The internal Standard
  LB resource itself is the one cost this provider cannot avoid; an idle internal
  LB is inexpensive but not free. `Check` flags a `Public` API server LB so the
  cheaper `Internal` default is a conscious choice.
- **Smallest burstable VM.** `Standard_B2s` (2 vCPU, 4 GB) by default — the
  smallest burstable size that clears kubeadm's two-CPU control-plane preflight.
  `azure.spot: true` requests Spot instances (dev/test only; a reclaimed Spot VM
  is deallocated).
- **Single control plane, no workers by default.** Node zero is adopted, not
  created, so for the default `--worker-count 0` CAPZ creates **no** VM of its
  own, and the only compute is node zero.

### Network details to validate live

The internal API server LB and node outbound connectivity are the parts most
worth checking on a first live run:

- Node zero reaches the internet through its own public IP (klotho attaches one),
  so it needs no NAT. **Workers** in a BYO subnet with an internal LB would need
  outbound (a NAT gateway, or their own public IPs); the cost-safe default has no
  workers. Add outbound before scaling `--worker-count`.
- Confirm CAPZ accepts the BYO vnet/subnet without trying to manage (or tag) it,
  and that the `control-plane`/`node` subnet roles both pointing at the single
  BYO subnet are accepted. If CAPZ rejects the shared subnet, give the template
  two subnet names.

The base image (Ubuntu 22.04 LTS) carries no Kubernetes, so create with
`--install-kubernetes`. No CNI is shipped; apply one after the API server is up.

## The config block

```yaml
capi:
  version: v1.12.9
providers:
  azure:
    version: v1.23.3
    # To serve this flat template, lay the CAPZ v1.23.3 release out as a local
    # clusterctl repository and point fetchURL at it (see "Serving the template"
    # below); componentsURL/image may point at the real release for the operator.
azure:
  subscriptionId: "<SUBSCRIPTION_ID>"
  tenantId: "<TENANT_ID>"
  clientId: "<CLIENT_ID>"
  # clientSecret: set AZURE_CLIENT_SECRET instead of storing it here
  location: eastus
  resourceGroup: klotho-rg        # existing RG
  vnet: klotho-vnet               # existing vnet in the RG
  subnet: klotho-subnet           # existing subnet in the vnet
  vmSize: Standard_B2s
  spot: true
  sshPublicKey: "ssh-ed25519 AAAA..."   # recommended, to diagnose a node
nodes:
  installKubernetes: true          # the image is a plain Ubuntu image
```

The client secret comes only from the environment:

```sh
export AZURE_CLIENT_SECRET=<the sp password>
```

## Serving the template to klotho

klotho fetches a provider's components and cluster template through clusterctl.
To use this flat template instead of CAPZ's default, lay the CAPZ v1.23.3 release
out as a local clusterctl repository and point `providers.azure.fetchURL` at it:

```
<dir>/infrastructure-azure/v1.23.3/
    infrastructure-components.yaml   # from the CAPZ v1.23.3 release, unchanged
    metadata.yaml                    # from the CAPZ v1.23.3 release, unchanged
    cluster-template.yaml            # this directory's cluster-template.yaml
```

```sh
base=https://github.com/kubernetes-sigs/cluster-api-provider-azure/releases/download/v1.23.3
mkdir -p repo/infrastructure-azure/v1.23.3 && cd repo/infrastructure-azure/v1.23.3
curl -sSL -O $base/infrastructure-components.yaml
curl -sSL -O $base/metadata.yaml
cp /path/to/klotho/provider/infrastructure/azure/cluster-template.yaml .
```

Set `fetchURL` to a `file://` URL of the components file (three slashes; a bare
Windows path is parsed as a URL scheme and rejected). Because a `file://` URL is
not reachable by the in-cluster operator, also set `providers.azure.componentsURL`
to the real release `infrastructure-components.yaml` so the operator can fetch
the provider manager.

## What was validated (offline, zero cost)

- `go build ./...`, `go vet ./...`, `go test ./...` green, including the azure
  package's faked-client tests for Prepare/Launch/Adopt/Plan/Teardown/Discover/
  Check, the customData cloud-init, the config resolvers, the base64 client
  secret, the create guard, and `providerSecretData`.
- The adoption, auth and endpoint design were read directly from CAPZ `v1.23.x`
  source (see "The adoption model" above), not guessed.

Not yet validated: a `--dry-run` through klotho's generator (it builds an Azure
client in `infra()`, which needs real credentials) and a live create.

## The live flow (create -> verify -> delete -> audit)

```sh
# 1. Create. klotho boots node zero (Spot), attaches a static public IP, installs
#    the CAPI operator + CAPZ from the local release, applies the flat bundle
#    (AzureClusterIdentity + Secret + AzureCluster with an Internal API LB),
#    adopts node zero, and pivots to self-managed.
klotho create aze2e --provider azure --config /abs/path/config.yaml \
  --kubernetes-version v1.31.4 --control-plane-count 1 --worker-count 0 \
  --install-kubernetes

# 2. Verify.
kubectl --kubeconfig <klotho-kubeconfig> get nodes
kubectl --kubeconfig <klotho-kubeconfig> get machines -A
# apply a CNI so the node goes Ready, e.g. kubectl apply -f <cilium/calico>

# 3. Delete, watching until it is gone (terminates the VM, deletes the NIC and
#    public IP, removes anything klotho created, by tag).
klotho delete aze2e --config /abs/path/config.yaml --watch

# 4. Audit to zero (below).
```

## Teardown and leak audit

**Tear down to zero by deleting only the cluster's resources, never the BYO
resource group.** klotho's Teardown removes the VM, NIC and public IP it created
(and any VM/NIC/public IP CAPZ created from the tagged template), all carrying
`klotho.io/cluster=<cluster>`. CAPZ's own Teardown removes the internal API
server LB and any other managed resources it added inside the BYO resource group.

Audit the resource group for anything left billing — a running VM, a managed
disk, a public IP, a NIC, or the internal load balancer:

```sh
rg=klotho-rg
# VMs still tagged for the cluster (want: empty)
az vm list -g $rg --query "[?tags.\"klotho.io/cluster\"=='aze2e'].name" -o tsv
# Public IPs tagged for the cluster (want: empty)
az network public-ip list -g $rg \
  --query "[?tags.\"klotho.io/cluster\"=='aze2e'].name" -o tsv
# NICs, managed disks and load balancers left in the RG (inspect)
az network nic list -g $rg -o table
az disk list -g $rg -o table
az network lb list -g $rg -o table
```

Do **not** `az group delete` the BYO resource group — it is pre-existing and may
hold the vnet/subnet and other resources klotho does not own.

## Approximate cost

A short CP-only run is a few cents of compute: one `Standard_B2s` **Spot** VM
(cents per hour), a 30 GB Standard_LRS OS disk (prorated, pennies for a short
run), one static Standard public IP (a few cents/day while allocated), and the
**internal** API server load balancer (an idle internal Standard LB is
inexpensive but not free). The ways to actually spend money here are to leave the
VM running, leave the public IP allocated, or scale workers (which need outbound).
Hence the audit above.
