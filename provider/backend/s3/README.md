# s3 state backend

Keeps each cluster's sealed state record as one object at
`<prefix>/<name>.record` in an S3-compatible bucket: AWS S3, MinIO, Ceph RGW,
Cloudflare R2 and the like. The bytes are already encrypted by klotho's state
codec before they reach the backend, so the store holds only opaque data and
never an encryption key.

Select it with `backend: s3` and an `s3:` section:

```yaml
backend: s3
s3:
  endpoint: http://minio.example:9000   # omit for AWS S3
  region: us-east-1
  bucket: klotho-state
  prefix: clusters
  usePathStyle: true                     # needed by MinIO
```

Credentials come from `KLOTHO_S3_ACCESS_KEY_ID` and
`KLOTHO_S3_SECRET_ACCESS_KEY`, or, when both are unset, the standard AWS
credential chain (environment, shared config, instance role). Keys are never
read from the configuration file.

## Reserving a name atomically

`Reserve` claims a cluster name by writing its object with a conditional
`PutObject` carrying `If-None-Match: *`. The store itself rejects the write
with `412 Precondition Failed` when an object already exists at the key, so
exactly one of several concurrent callers wins the name and the rest get
`false` without error. No read-then-write is involved, so there is no race
between callers.

**This depends on the store honouring conditional writes.** AWS S3 supports
`If-None-Match: *` on `PutObject`, as do recent MinIO, Ceph RGW and Cloudflare
R2. A store that silently ignores the condition would let two callers both
"win" a `Reserve` and overwrite each other — use such a store only where a
single operator ever creates clusters, or not at all. `Save` is always
last-writer-wins: there is no compare-and-swap.

`Delete` returns `ErrNotFound` when the record is absent, matching the file and
proxmox backends. S3's own `DeleteObject` is idempotent and reports nothing
about prior existence, so `Delete` first confirms the object with `HeadObject`.
