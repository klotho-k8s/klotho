# klotho - Spin the first thread of your Kubernetes cluster.

Designed by Luther Monson in Arizona 🌵 Assembled in [Claude Opus](https://claude.ai).

## Etymology

*klotho*, from the Greek κλώθω, "to spin." It is the name of
[Klotho](https://en.wikipedia.org/wiki/Clotho) (Κλωθώ), youngest of the three
Fates, the spinner who draws the thread of a life from her distaff and winds
it onto her spindle. Her sisters only measure the thread and cut it. She alone
begins it. The k is for Kubernetes.

klotho does the same for a cluster. It spins the first thread, a single node
with its certificates and its control plane, and winds the rest of the cluster
onto it one turn at a time, until the thing is alive and spinning on its own.
Then, like the Fate who only ever starts the thread, it steps back and leaves
the cluster to live its own life.

## Build

Builds run with [mage](https://magefile.org). Install it once with `go install
github.com/magefile/mage@latest`, then:

```sh
mage build    # produces bin/klotho
mage -l       # list every target
```

Without mage on PATH, `go run github.com/magefile/mage build` works too.

## Test

```sh
mage test
```

The S3 payload host also has an end-to-end test that boots MinIO in podman.
It is skipped unless asked for:

```sh
KLOTHO_E2E_S3=1 go test ./provider/payload/s3/ -run E2E -v
```

## Configuration

klotho reads `$XDG_CONFIG_HOME/klotho/config.yaml` (`~/.config/klotho/config.yaml`),
merges an optional `--profile` over it, then applies environment variables.
`klotho init` writes a commented skeleton. Every field is optional.

### Payload host (S3)

A payload host serves files that are too large for the infrastructure
provider's user-data channel. The bootstrap node fetches each one through a
presigned URL with a bounded lifetime, so nothing in user-data carries a
credential. Without a `payload` section no host is used and everything is
inlined.

The S3 host works with AWS S3 and anything that speaks its API, such as
MinIO, Ceph RGW or Cloudflare R2:

```yaml
payload:
  kind: s3
  s3:
    endpoint: http://minio.example:9000   # omit for AWS S3
    region: us-east-1                     # default us-east-1
    bucket: klotho                        # required; must already exist
    prefix: payload                       # optional key prefix
    usePathStyle: true                    # bucket in the path, not the host name; MinIO needs it
    presignTTL: 1h                        # how long a fetch URL stays valid; default 1h
```

Objects are written to `<prefix>/<cluster>/<path on the node>` and removed
again when the cluster is deleted.

Credentials come from the environment only; the file does not accept them:

| Variable | Purpose |
| --- | --- |
| `KLOTHO_S3_ACCESS_KEY_ID` | access key |
| `KLOTHO_S3_SECRET_ACCESS_KEY` | secret key |

Set both or neither. When neither is set the standard AWS credential chain
applies (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, `~/.aws/credentials`,
an instance or task role, and so on).

### Generated bundles and what klotho patches into them

klotho never edits a manifest you pass with `--manifest`. A generated bundle
gets only klotho's own plumbing (the control plane endpoint, the per-cluster
tag, the pool) plus the explicit choices below. Each one is off unless you
set it, and each is applied by the same patch step that sets the endpoint.

#### Proxmox templates selected by tag

```yaml
proxmox:
  template:
    tags: [k8s, ubuntu-24.04]   # Proxmox tags: letters, digits, _ . + -
    matchPolicy: exact          # exact (default) | uniqueSubset | bestSubset
```

With `tags` set, every `ProxmoxMachineTemplate` of a generated bundle gets
`spec.template.spec.templateSelector.matchTags` (and `templateMatchPolicy`
when it is not `exact`) and loses its fixed `templateID` and `sourceNode`.
`exact` wants a template whose tags equal the list; `uniqueSubset` wants the
only template carrying all of them, extra tags allowed; `bestSubset` takes the
one with the fewest extra tags.

The Cluster API Proxmox provider resolves the selector **once across the whole
cluster**: the tags must pick exactly one template, and every machine is
cloned from that template to whichever node the scheduler chooses among
`allowedNodes`. A clone to a node other than the template's own only works
when the template's disks are on storage every allowed node shares (NFS, CIFS,
iSCSI, Ceph, and so on); a template on `local` or `local-lvm` can only serve
its own node. `create` reports both problems as findings before it starts: a
selector that matches nothing or more than one template, and a template on
local storage while other nodes are allowed. The bootstrap node is cloned from
the template the tags select on its node, or, when there is none there, from
the cluster-wide match if its storage is shared. `templateId` may stay set
alongside, as long as it is the template the tags select.

So the two working layouts are one tagged template on shared storage, which
lets machines spread across all allowed nodes, or one tagged template on local
storage with `allowedNodes` restricted to its node.

The template must not carry a Proxmox cloud-init drive (`ide2:
<storage>:vm-<id>-cloudinit`). The provider attaches its own cloud-init ISO to
each clone and leaves that drive in place, so the guest sees two datasources
and may take the template's, booting with no network and no bootstrap. klotho
strips the drive from its own bootstrap clone and reports it as a finding for
the template the tags select; remove it with `qm set <id> --delete ide2`.

#### Proxmox API endpoints

```yaml
proxmox:
  url: https://pve1.example:8006
  additionalUrls: [https://pve2.example:8006, https://pve3.example:8006]
  endpointDiscovery: true
```

The Cluster API Proxmox provider running in the cluster is given `url` and
talks to that one node, which forwards requests about the other nodes. When
that node is down, or cannot reach the node a request is about, the request
fails although the rest of the cluster could have served it. `additionalUrls`
names further API URLs of the same cluster for the provider to fall back to,
and `endpointDiscovery` makes it add every online node of the cluster on its
own, read from the cluster status through `url` (the token needs `Sys.Audit`
on `/`). Both are off unless set, and both are passed to the provider as its
configuration; klotho's own calls still go to `url`.

#### Installing Kubernetes at boot

```yaml
nodes:
  installKubernetes: true       # or: klotho create --install-kubernetes
  cache:                        # optional, used by the install script only
    aptProxy: http://cache.lan:3142
    registryMirrors:
      registry.k8s.io: [http://cache.lan:5000]
      docker.io: [http://cache.lan:5001]
```

`installKubernetes` (or `--install-kubernetes`; `--install-kubernetes=false`
overrides the file) adds `/usr/local/bin/klotho-install-kubernetes.sh` to the
`KubeadmControlPlane`'s `kubeadmConfigSpec.files` and to every worker
`KubeadmConfigTemplate`, and puts it first in their `preKubeadmCommands`, so
a plain Debian or Ubuntu image gets containerd (with `SystemdCgroup`), the
kernel modules and sysctls, swap off, and `kubelet`/`kubeadm`/`kubectl` pinned
to the bundle's Kubernetes version from `pkgs.k8s.io` before kubeadm runs. A
node that already has that kubeadm version exits the script at once, so
prepared templates are unaffected. It logs to
`/var/log/klotho-install-kubernetes.log`, retries apt a few times while the
network settles, and falls back to the minor's latest release (saying so in
the log) when the exact patch is not packaged.

`cache.aptProxy` makes the script write `/etc/apt/apt.conf.d/01klotho-proxy`
(the proxy must pass `https` through for `pkgs.k8s.io`). `cache.registryMirrors`
makes it write a `hosts.toml` per registry under `/etc/containerd/certs.d`
and point containerd's registry `config_path` there. Both ride in the script
and do nothing without `installKubernetes`.

## Design notes

Longer explanations of the decisions behind individual packages live next to
the code:

- `provider/backend/proxmox/README.md`: why cluster state is kept in the
  comment of an empty Proxmox resource pool, what was rejected, and the limits.
- `provider/payload/host.go`: why a payload host exists, how large klotho's
  user-data actually is, which providers' user-data caps it exceeds, and how
  to measure your own. The S3 host is implemented and tested; the create
  flow inlines everything on providers whose channel has no cap, and wiring
  the host into create is left for the first provider that needs it.

