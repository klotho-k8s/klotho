# vault state backend

Keeps each cluster's sealed state record as one secret at
`<mount>/<prefix>/<name>` in a HashiCorp Vault **KV v2** secrets engine. The
record is base64-encoded into a single `record` field, so an opaque record of
arbitrary bytes survives KV v2's JSON transport untouched. The bytes are already
age-encrypted by klotho's state codec before they reach the backend, so the
store holds only opaque data and never an encryption key.

What Vault adds on top of the bytes is the point of this backend: IAM-scoped
access to the path, an audit trail of every read and write, and rotation of the
path's own credentials.

Select it with `backend: vault` and a `vault:` section:

```yaml
backend: vault
vault:
  address: https://vault.example:8200   # omit to use VAULT_ADDR
  mount: secret                          # KV v2 mount path; default "secret"
  prefix: klotho/clusters                # path prefix below the mount
  namespace: team-a                      # Vault Enterprise / HCP namespace, optional
  insecure: false                        # skip TLS verification; development only
```

## Authentication

The baseline is the standard Vault environment:

| Variable | Purpose |
| --- | --- |
| `VAULT_ADDR` | server URL (when `address` is unset) |
| `VAULT_TOKEN` | the token every request carries |
| `VAULT_NAMESPACE` | namespace (when `namespace` is unset) |

The token comes from the environment only; the configuration file does not
accept it (a `token:` key in the file is rejected). The client is structured so
another auth method — AppRole, Kubernetes, and so on — can obtain and set the
token before the store is used; token auth is enough for the first version.

## KV v2 path handling

KV v2 does not store a secret at its bare path. It splits the API path by
operation:

- read and write go to `<mount>/data/<prefix>/<name>`;
- list and delete-all-versions go to `<mount>/metadata/<prefix>/<name>`.

The store builds both forms from the configured mount and prefix. `List` issues
a metadata `LIST` against `<mount>/metadata/<prefix>` and keeps only the
immediate child keys that are valid cluster names, so a nested folder or a
secret a person wrote by hand is never mistaken for a cluster.

## Reserving and saving atomically with check-and-set

KV v2 supports **check-and-set** (`cas`) on writes: a write carries the version
it expects to be replacing, and Vault rejects it when the current version
differs.

- `Reserve` writes with `cas=0`, which Vault accepts only when **no** version of
  the secret exists yet. Exactly one of several concurrent callers wins the
  name; the rest get a check-and-set rejection, which `Reserve` reports as
  `false` without error. No read-then-write is involved, so there is no race
  between callers.
- `Save` reads the current version and writes with `cas` set to it. If a
  concurrent writer moved the version in between, Vault rejects the write and
  `Save` re-reads and retries (a bounded number of times), so an update is never
  silently lost to a last-writer-wins overwrite.

`Delete` removes every version and the metadata of the secret. A metadata
`DELETE` is idempotent and reports nothing about prior existence, so `Delete`
first confirms the metadata is there to return `ErrNotFound` when the record is
absent, matching the file, s3 and proxmox backends.

> KV v2 is required: the engine must be version 2 (the default `secret/` mount
> on a dev-mode Vault is). A KV v1 mount has no versions and no `cas`, so the
> atomicity guarantees above do not hold against it.

## Testing against a real Vault

Unit tests run against an in-memory fake of the KV v2 logical API. There is also
a gated end-to-end test that boots a dev-mode Vault (`vault server -dev`) — no
container, so it never touches podman — and drives the real store against it. It
is skipped unless `KLOTHO_E2E_VAULT=1` and a `vault` binary is on PATH:

```sh
KLOTHO_E2E_VAULT=1 go test ./provider/backend/vault/ -run E2E -v
```

`KLOTHO_E2E_VAULT_BIN` overrides the binary and `KLOTHO_E2E_VAULT_PORT` the
loopback port. A dev-mode Vault keeps everything in memory and is thrown away
when the test ends.
