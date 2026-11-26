# Testing the AWS (CAPA) provider cheaply

This is a recipe for exercising klotho's AWS provider against a real AWS account
without running up a bill. It uses a network design that has **no hourly-billed
infrastructure at all** - no NAT gateway, no managed load balancer - and spot
instances for the nodes, so a full test costs a few cents.

It documents what has been validated so far (the template and the whole bundle
generation, offline) and the live create/verify/delete/audit flow with the exact
commands. Where something has not yet been run live, it says so.

## The cheap-network design, and why

A default CAPA cluster is expensive to leave running: it creates a VPC with a
**NAT gateway** (~$0.045/hr + data) so private nodes can reach the internet, and
an **NLB** (~$0.0225/hr + LCUs) for the API server. Both bill by the hour whether
or not you use them, and a leaked one is easy to forget. This recipe removes both:

- **Single AZ, bring-your-own public subnet + internet gateway, no NAT.** klotho
  requires an existing VPC and subnet (`aws.vpcId`/`aws.subnetId`); it does not
  create a network. Point it at a **public** subnet (one whose route table has a
  `0.0.0.0/0` route to an internet gateway). The default VPC every account has
  already is exactly this. With a public subnet the nodes reach the internet
  directly, so **no NAT gateway is needed**.
- **No managed load balancer.** The template sets
  `AWSCluster.spec.controlPlaneLoadBalancer.loadBalancerType: disabled`. CAPA then
  does not create an NLB/ELB; instead it waits for `spec.controlPlaneEndpoint` to
  be set externally. That is precisely klotho's model: klotho boots node zero,
  allocates an **Elastic IP**, associates it with node zero, and uses that EIP as
  the API endpoint. An EIP is **free while it is associated with a running
  instance** (you are billed only for an EIP left allocated but unattached - see
  the audit step).
- **`publicIP: true` on every machine** so a node in the public subnet gets a
  routable address and needs no NAT to pull images.
- **`spotMarketOptions: {}`** so both node zero and any CAPA-managed machines run
  as one-time spot instances (dev/test only; a reclaimed spot node is terminated,
  not restarted).
- **`cloudInit.insecureSkipSecretsManager: true`** so bootstrap data travels as
  EC2 user-data instead of AWS Secrets Manager - no Secrets Manager IAM or API
  calls to set up or pay for.

The template is [`cluster-template.yaml`](./cluster-template.yaml) in this
directory. It is authored in the Cluster API **v1beta2 contract** (object
references use `apiGroup`, not `apiVersion`), which is what klotho's generator
requires; CAPA's own published `cluster-template.yaml` is still v1beta1-contract
and will be rejected by klotho with `KubeadmControlPlane v1beta1 is not
supported, want v1beta2`.

## Serving the template to klotho

klotho fetches a provider's components and cluster template through clusterctl.
To use this flat template instead of CAPA's default, lay the CAPA v2.8.2 release
out as a local clusterctl repository and point `providers.aws.fetchURL` at it:

```
<dir>/infrastructure-aws/v2.8.2/
    infrastructure-components.yaml   # from the CAPA v2.8.2 release, unchanged
    metadata.yaml                    # from the CAPA v2.8.2 release, unchanged
    cluster-template.yaml            # this directory's cluster-template.yaml
```

```sh
base=https://github.com/kubernetes-sigs/cluster-api-provider-aws/releases/download/v2.8.2
mkdir -p repo/infrastructure-aws/v2.8.2 && cd repo/infrastructure-aws/v2.8.2
curl -sSL -O $base/infrastructure-components.yaml
curl -sSL -O $base/metadata.yaml
cp /path/to/klotho/provider/infrastructure/aws/cluster-template.yaml .
```

Then set `fetchURL` to a **`file://` URL** of the components file (a bare Windows
path like `C:/...` is parsed as a URL scheme and rejected; use three slashes):
`file:///C:/.../repo/infrastructure-aws/v2.8.2/infrastructure-components.yaml`.

## The config block

This is the `aws:` block for a live run (allocate an EIP as the endpoint). It is
the real configuration used to validate the template; the only change from the
offline dry-run config (below) is `allocateElasticIp: true` in place of a fixed
`controlPlaneEndpoint`.

```yaml
capi:
  version: v1.12.9
providers:
  aws:
    version: v2.8.2
    fetchURL: file:///ABSOLUTE/PATH/repo/infrastructure-aws/v2.8.2/infrastructure-components.yaml
aws:
  region: us-west-2
  vpcId: vpc-xxxxxxxx           # a VPC with a public subnet
  subnetId: subnet-xxxxxxxx     # a PUBLIC subnet (0.0.0.0/0 -> igw-...)
  amiId: ami-xxxxxxxxxxxxxxxxx  # an Ubuntu 22.04 image in the region
  instanceType: t3.medium       # spot
  sshKeyName: your-ec2-keypair
  securityGroupIds: [sg-xxxxxxxx] # a group allowing 6443/22 in and node-to-node
  allocateElasticIp: true       # free while associated; the API endpoint
  publicIp: true                # public subnet, no NAT
  spot: true
nodes:
  installKubernetes: true        # the AMI is a plain Ubuntu image, not a k8s image
```

The security group should allow inbound `6443` (so you can reach the API and, if
you add workers, they can join) and `22` (to diagnose a node), and all traffic
from itself (node-to-node). node zero is launched into `securityGroupIds`.

## Validated so far (offline, zero cost)

The template and klotho's entire bundle-generation path are validated with a
`--dry-run`, which renders every manifest and the node user-data **without
creating anything in AWS** (it allocates no EIP when `controlPlaneEndpoint` is
set, and makes only read-only `Describe*` calls to validate the VPC/subnet/key
and resolve the AMI):

```sh
# dry-run config: same as above but with a placeholder endpoint instead of
# allocateElasticIp, so Prepare provisions nothing.
#   controlPlaneEndpoint: 203.0.113.10   # TEST-NET placeholder
#   (remove allocateElasticIp)
klotho create awse2e --provider aws --config /abs/path/config.yaml \
  --kubernetes-version v1.31.4 --control-plane-count 1 --worker-count 1 \
  --install-kubernetes --dry-run ./out
```

This produced a correct flat bundle: `AWSCluster` with
`loadBalancerType: disabled`, the BYO `vpc`/`subnets`, the EIP patched onto
`spec.controlPlaneEndpoint`, and both `AWSMachineTemplate`s carrying
`publicIP: true`, `spotMarketOptions: {}` and `insecureSkipSecretsManager: true`,
plus the control-plane adoption object, the CAPI operator/provider install
manifest, and node zero's user-data. An AWS audit afterward confirmed **zero**
instances and **zero** EIPs.

## The live flow (create -> verify -> delete -> audit)

> Status: the template and bundle are validated offline as above. The live
> create has **not** been run end-to-end yet; two things are known to be needed
> first (see "Known blockers"). The commands are the intended flow.

```sh
# 1. Create. klotho boots node zero (spot), allocates + associates an EIP,
#    installs the CAPI operator + CAPA from the local release, applies the flat
#    bundle, adopts node zero, and pivots to self-managed.
klotho create awse2e --provider aws --config /abs/path/config.yaml \
  --kubernetes-version v1.31.4 --control-plane-count 1 --worker-count 0

# 2. Verify. klotho writes an admin kubeconfig; check the API and the node.
kubectl --kubeconfig <klotho-kubeconfig> get nodes
kubectl --kubeconfig <klotho-kubeconfig> get machines -A

# 3. Delete, watching until it is gone (token-only sweep; terminates the
#    instance, releases the EIP, removes anything klotho created).
klotho delete awse2e --config /abs/path/config.yaml --watch

# 4. Audit to zero (below).
```

### Known blockers for a full live cluster on stock CAPA v2.8.2

1. **Control-plane machine *creation* is broken when the LB is disabled.** CAPA
   issue #6017 / PR #6018 (unmerged as of v2.8.2): with
   `loadBalancerType: disabled`, CAPA's AWSMachine create path still waits on the
   (never-populated) APIServer ELB and fails with `failed to run controlplane,
   APIServer ELB not available`. klotho's model *adopts* node zero (the instance
   already exists, so the create path is skipped), which is expected to dodge
   this - but **scaling the control plane** (`--control-plane-count` > 1, or any
   KCP-driven replacement) will hit it. A multi-CP cluster needs a CAPA build
   carrying #6018. Workers are not control-plane and are not affected.
2. **No CNI.** Like all CAPA templates, this one ships no CNI, so nodes stay
   `NotReady` until one is applied. Apply your choice after the API is up, e.g.
   `kubectl apply -f <calico/cilium manifest>`.

## Teardown and leak audit

Billable leaks after an AWS test are, in order of what to check: **running/stopped
instances**, **unattached Elastic IPs** (an EIP not associated with a running
instance bills ~$0.005/hr), and - only if a run used CAPA's managed network -
**NAT gateways** and **load balancers**. This recipe creates none of the last
two, but the audit sweeps for them anyway in case a managed-network run leaked
one.

`scratchpad/aws-e2e-teardown.sh` force-deletes everything tagged
`klotho-e2e=<cluster>` or CAPA-owned for the cluster, in dependency order (LBs +
target groups, instances, NAT gateways, EIPs, then security groups), and is safe
to re-run. Audit to zero with:

```sh
region=us-west-2 cl=awse2e
aws --region $region ec2 describe-instances \
  --filters "Name=tag:klotho.io/cluster,Values=$cl" \
  "Name=instance-state-name,Values=pending,running,stopping,stopped" \
  --query 'Reservations[].Instances[].InstanceId' --output text   # want: empty
aws --region $region ec2 describe-addresses \
  --filters "Name=tag:klotho.io/cluster,Values=$cl" \
  --query 'Addresses[].AllocationId' --output text                # want: empty
```

## Approximate cost

A short CP-only run is a few cents: one `t3.medium` **spot** instance
(~$0.012-0.02/hr in us-west-2, so under 1 cent for a 20-30 min test), the EIP
**free** while associated, and no NAT or LB. The only way to actually spend money
here is to leave an instance running or leave an EIP allocated but unattached -
hence the audit above.
