# azureblob state backend

Keeps each cluster's sealed state record as one block blob at
`<prefix>/<name>.record` in an Azure Blob Storage container. The bytes are
already encrypted by klotho's state codec before they reach the backend, so the
store holds only opaque data and never an encryption key.

Select it with `backend: azureblob` and an `azureblob:` section:

```yaml
backend: azureblob
azureblob:
  accountName: klothostate              # storage account
  container: klotho-state               # must already exist
  prefix: clusters                      # optional blob-name prefix
  endpoint: http://127.0.0.1:10000/devstoreaccount1   # optional; override for Azurite
```

The blob service URL is `https://<accountName>.blob.core.windows.net` unless
`endpoint` overrides it (for the Azurite emulator, a sovereign cloud, or a
private endpoint).

## Authentication

Credentials come from the environment, never from the configuration file:

| Variable | Purpose |
| --- | --- |
| `KLOTHO_AZURE_STORAGE_CONNECTION_STRING` | full account connection string |
| `KLOTHO_AZURE_STORAGE_ACCOUNT_KEY` | shared account key for `accountName` |

Resolution order:

1. A connection string, if set, is used as-is (it already carries the account,
   key and endpoint).
2. Otherwise a shared account key, if set, is paired with `accountName`.
3. Otherwise `DefaultAzureCredential`: the standard Azure chain of environment
   service principal (`AZURE_CLIENT_ID` / `AZURE_TENANT_ID` /
   `AZURE_CLIENT_SECRET`), workload identity, managed identity, the Azure CLI,
   and so on. `accountName` (or `endpoint`) is then required to form the service
   URL.

A key or connection string placed in the configuration file is rejected: those
fields are environment-only.

## Reserving a name atomically

`Reserve` claims a cluster name by uploading its blob with an
`If-None-Match: *` access condition. The service itself rejects the write when
a blob already exists at the name — Azure answers with `409 BlobAlreadyExists`
or `412 ConditionNotMet` depending on the path — so exactly one of several
concurrent callers wins the name and the rest get `false` without error. No
read-then-write is involved, so there is no race between callers.

Unlike some S3-compatible stores, Azure Blob honours this conditional write
unconditionally, so `Reserve` is always atomic here.

`Save` is last-writer-wins: there is no compare-and-swap. Azure Blob offers
per-blob optimistic concurrency with an ETag `If-Match` condition (and
exclusive blob leases) that could enforce one, but the `backend.Store`
contract's `Save` takes no expected version, so none is attempted — matching the
s3 backend.

`Delete` returns `ErrNotFound` when the record is absent, matching the file,
proxmox and s3 backends. Azure's own blob delete fails on a missing blob, so
`Delete` first confirms the blob is present with a properties check.

## Testing

Unit tests run against an in-memory fake of the blob client, so they never dial
Azure. An end-to-end test boots the Azurite emulator in podman and drives the
real client against it; it is skipped unless asked for:

```sh
KLOTHO_E2E_AZURITE=1 go test ./provider/backend/azureblob/ -run E2E -v
```

The container is removed when the test ends, pass or fail. The image is left
behind; remove it with `podman rmi` when it is no longer wanted.
