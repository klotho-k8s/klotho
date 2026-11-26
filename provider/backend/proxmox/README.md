# Proxmox state backend

This package is a `backend.Store` that keeps each cluster's state record
inside the Proxmox VE cluster the machines run on. There is no second system
to stand up and no second credential to manage: the same API token that
creates the virtual machines reads and writes the state.

## What is stored, and where

One **resource pool per cluster**, named `klotho-<cluster>` by default. The
pool is normally empty. The state record, an opaque single line produced by
the caller's codec (age-encrypted by default, with a `klotho/v1:` marker),
lives in the pool's **comment** field.

A sealed record for a one-node kubeadm cluster is about 10 KB (measured
2026-11: a 5.3 KB plain JSON state becomes a 9.8 KB age record). The probe
in `probe.go` writes and reads back comments of 1 KB to 128 KB; on Proxmox
VE 9.2 every size round-trips byte for byte, newlines included. That leaves
an order of magnitude of headroom.

## Why an empty resource pool

The store needs three things from its container: an atomic "create this
name or fail" so two people cannot both bootstrap `prod`, a field that can
hold a few tens of kilobytes and be read back, and visibility from every
node. A pool is the only Proxmox object that gives all three.

- **Creating a pool is atomic across the whole Proxmox cluster.** Pool IDs
  are unique, and `POST /pools` fails if the ID exists. That one call is
  the name reservation, with no lock service and no compare-and-swap of our
  own. Exactly one of several concurrent `klotho create prod` wins.
- **The comment is a real read-write field with room.** It is stored in the
  cluster configuration, replicated by pmxcfs to every node, and readable
  through the API from any of them. Nothing in the API documentation caps
  it, and the probe shows 128 KB works.
- **It is visible to humans where they already look.** A pool named
  `klotho-prod` appears in the Proxmox UI tree, so an administrator can see
  which clusters exist without klotho, and can see at a glance that the
  pool holds a record (its comment starts with the marker).
- **It is the natural scope for everything else.** Machines can optionally
  be placed in the pool, which turns it into the teardown and listing
  boundary, and a future per-cluster API token can be granted rights on
  `/pool/klotho-prod` alone.

### What was rejected

- **VM notes.** Capped at 8 KB, which the encrypted record already exceeds,
  and tied to a VM that CAPI may replace or delete. State must outlive any
  one machine.
- **Datacenter notes.** A single 64 KB field shared by every cluster and
  every administrator's own notes. Two clusters saving at once would race,
  and there is no atomic reservation in it.
- **Snippets, ISOs, disks.** Content on storage can be uploaded but not
  read back through the API, so it cannot hold state at all. Storage is
  also per node unless shared.
- **A VM's description, tags or SMBIOS fields.** Same objections as VM
  notes, plus tag character restrictions.
- **Anything outside Proxmox.** Possible (the `file` backend exists, S3 is
  planned) but it is a second system and, for a lab, a second thing to
  back up and secure.

## Guards

- A pool whose comment does not start with a record marker is **never**
  treated as a cluster: `Load` and `Delete` return `ErrNotAKlothoPool`,
  `List` skips it. A pool somebody made by hand is safe from klotho.
- `Delete` detaches any VM or container members, then removes the pool, and
  only when the record marker is present. Storage members are never
  touched: a pool that holds storage is refused until a person removes it.
- `Save` replaces the comment in one API call. There is no compare-and-swap,
  so two writers for the same cluster are last-writer-wins. `Reserve` is
  the only operation that needs to be atomic, and it is.

## Security posture

The API token can read every pool comment. It cannot decrypt one: the
record is sealed with age to the recipients configured in klotho, and the
cluster CA key inside it is what mints kubeconfigs. Read access to Proxmox
is therefore not admin access to the clusters. Keep the age identity
separate from the Proxmox token.

## Limits to keep in mind

- Pool IDs allow letters, digits, `.`, `-` and `_`, at most 63 characters
  including the prefix. Cluster names are validated to fit.
- Pool comments live in the cluster-wide `user.cfg`, which pmxcfs keeps
  under a per-file size limit (1 MiB on current releases). A handful of
  10 KB records is nothing; thousands of clusters on one Proxmox cluster
  would be the point to move state elsewhere.
- Comments are plain text in the Proxmox configuration backups. The record
  is encrypted, so that is fine, but it is one more place the ciphertext
  lives.
