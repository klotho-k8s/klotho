# gcs state backend

Keeps each cluster's sealed state record as one object at
`<prefix>/<name>.record` in a Google Cloud Storage bucket. The bytes are
already encrypted by klotho's state codec before they reach the backend, so the
store holds only opaque data and never an encryption key.

Select it with `backend: gcs` and a `gcs:` section:

```yaml
backend: gcs
gcs:
  bucket: klotho-state
  prefix: clusters
  # credentialsFile: /etc/klotho/sa.json   # a path, not a key (optional)
```

## Authentication

Credentials come from Application Default Credentials: the service-account key
JSON at `GOOGLE_APPLICATION_CREDENTIALS`, a `gcloud auth application-default
login` session, or the attached service account of the GCE/GKE/Cloud Run host
klotho runs on. `gcs.credentialsFile` names a service-account key JSON
explicitly; it falls back to `GOOGLE_APPLICATION_CREDENTIALS` when unset. It is
a *path*, not a credential, so it may live in the configuration file — the key
contents never do.

The backend needs `storage.objects.{get,create,delete,list}` on the bucket
(for example the `roles/storage.objectAdmin` role scoped to it). The bucket
must already exist; klotho does not create it.

## Reserving a name atomically

`Reserve` claims a cluster name by creating its object with a `DoesNotExist`
generation precondition — GCS's compare-and-swap. The store itself rejects the
write with `412 Precondition Failed` when a live object already exists at the
key, so exactly one of several concurrent callers wins the name and the rest
get `false` without error. No read-then-write is involved, so there is no race
between callers. This is the GCS analog of the s3 backend's `If-None-Match: *`.

GCS also offers `IfGenerationMatch` for a true read-modify-write compare-and-
swap against a known object generation. The `backend.Store` interface carries
no generation through `Save`, so `Save` here is last-writer-wins, exactly like
the s3 backend; the generation preconditions are used only for the atomic
`Reserve`.

`Delete` returns `ErrNotFound` when the record is absent, matching the file,
proxmox and s3 backends. Unlike S3, GCS's own `Delete` reports whether the
object existed, so no prior `Head`/`Attrs` call is needed to keep that
contract.

## Testing

Unit tests run against an in-memory fake of the small GCS surface the store
uses, so `go test ./provider/backend/gcs/` dials nothing.

An end-to-end test (`-run E2E`, gated on `KLOTHO_E2E_GCS=1`) drives the real
storage client against a running
[`fsouza/fake-gcs-server`](https://github.com/fsouza/fake-gcs-server)
emulator. Start the emulator yourself — the test launches no container — and
point `STORAGE_EMULATOR_HOST` (or `KLOTHO_E2E_GCS_HOST`, default
`localhost:4443`) at it:

```sh
podman run --rm -p 4443:4443 fsouza/fake-gcs-server \
  -scheme http -backend memory -public-host localhost:4443
KLOTHO_E2E_GCS=1 go test ./provider/backend/gcs/ -run E2E -v
```
