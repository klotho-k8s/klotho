# kubernetes (Secret) state backend

Keeps each cluster's sealed state record in a Kubernetes **Secret**, named
`klotho-state-<cluster>`, in a configurable namespace of a **target** cluster.
The record is stored Opaque under a single data key (`record`). The bytes are
already encrypted by klotho's state codec before they reach the backend, so the
Secret holds only opaque data and never an encryption key.

## The target cluster must already exist — it is NOT the cluster being created

**This backend needs a separate, already-running Kubernetes cluster that the
klotho CLI can reach** (its kubeconfig and context come from configuration).

**It cannot be the cluster klotho is bootstrapping.** klotho writes a cluster's
state *as it creates it* — the reservation is written before the first node
boots — so if the backend pointed at the cluster being created, there would be
no API server to write to yet. That is a chicken-and-egg deadlock, and it never
resolves. Point this backend only at a standing **management or utility
cluster** that is up before any `klotho create` runs.

This makes the backend a natural fit for teams who already operate a management
cluster, and it dovetails with adopting existing clusters: the same cluster that
runs your tooling holds the state for the workload clusters klotho builds.

## Selecting it

```yaml
backend: kubernetes
kubernetes:
  kubeconfig: ~/.kube/management-config   # omit for KUBECONFIG, then ~/.kube/config
  context: mgmt                           # omit for the kubeconfig's current-context
  namespace: klotho-state                 # omit for "default"
```

klotho reaches the target cluster with the same credentials your kubeconfig
already carries; nothing extra is stored in klotho's config. Give the identity
`create`, `get`, `update`, `delete` and `list` on Secrets in that namespace.

## Reserving a name atomically

`Reserve` claims a cluster name by **creating** its Secret. The API server
itself rejects a second `Create` of the same name with `AlreadyExists`, so
exactly one of several concurrent callers wins the name and the rest get `false`
without error. No read-then-write is involved, so there is no race between
callers — the same guarantee the file backend gets from `O_CREATE|O_EXCL` and
the s3 backend from `If-None-Match: *`.

## Locking: optimistic concurrency on `resourceVersion`

Unlike the file and s3 backends, whose `Save` is last-writer-wins, this backend
does a **compare-and-swap**. `Save` reads the Secret's `resourceVersion` and
issues the `Update` carrying it; the API server rejects the write with a
`Conflict` when another caller changed the Secret in the meantime, and `Save`
re-reads the current Secret and retries (bounded, a handful of attempts). Two
concurrent `Save`s therefore never silently lose a write — the loser retries
against the winner's version. When no Secret exists yet, `Save` creates it (and
retries as an update if it loses the create race).

`Delete` returns `ErrNotFound` when the Secret is absent, matching the file and
s3 backends.

`List` selects only the Secrets this backend labels
(`app.kubernetes.io/managed-by=klotho`,
`app.kubernetes.io/component=cluster-state`) and reads the cluster name from the
`klotho.sh/cluster` label, so a Secret a person placed in the namespace by hand
is never mistaken for a cluster.

## Cluster-name constraint

A Secret name is a DNS subdomain, so this backend's cluster names are stricter
than the others': **lowercase** letters, digits, `-` and `.`, starting and
ending with an alphanumeric, at most 63 characters, and **no underscores or
uppercase**. `klotho-state-<name>` must be a valid Secret name and `<name>` a
valid label value; names that are not are rejected before any request is sent.
