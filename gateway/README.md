# gateway

The S3-compatible gateway (`simplyblock-s3-interface-spec.pdf`, Phase 1): an
embedded [versitygw](https://github.com/versity/versitygw)
(`github.com/versity/versitygw/embedgw`) over its own `backend/posix`, serving
one bucket per simplyblock volume via `--bucketlinks`. The `ObjectStore`
reconciler that deploys this as a Kubernetes workload is
`operator/docs/designs/design-objectstore.md`.

## Why bucket-per-volume

Each S3 bucket is backed by its own simplyblock logical volume, formatted as
its own XFS filesystem; the bucket's entry under the gateway's root directory
is a symlink to that volume's mount point, which `--bucketlinks` makes
`versitygw` resolve and list instead of rejecting. This maps buckets directly
onto the existing pool model, giving per-bucket QoS, snapshots, capacity, and
— most importantly — per-bucket disaster-recovery consistency-group
membership (buckets failing over alongside VM disks and databases under one
`DRPolicy`), which a single shared filesystem for every bucket cannot: one QoS
class, one capacity pool, one DR domain for every tenant's data, for
simplicity of provisioning this feature doesn't need.

Confirmed from `versitygw` source: `--bucketlinks` only governs a bucket's own
top-level entry, resolved via direct `Lstat`/`Stat`, not the directory-walk
code path behind `ListObjects`'s documented "does not traverse symlinks"
behavior — the two are unrelated, so this works for
`ListBuckets`/`ListObjects(V2)`/`GetObject`/`PutObject` without qualification.

## Why no backend or IAM code lives here

`github.com/versity/versitygw/backend/posix` already implements
`backend.Backend` as a complete POSIX filesystem backend, bucket-per-volume
symlinks and durable `PutObject` included (`--bucketlinks`, `--fsync`: see
above, and `internal/config/config.go`'s `Fsync` field — `versitygw` leaves
durability off by default, this project does not). IAM is versitygw's
built-in Vault backend, used as-is, configured through versitygw's own
`VGW_IAM_VAULT_*` variables. Without `VGW_IAM_VAULT_ENDPOINT_URL`, `embedgw`
falls back to a single-account `auth.IAMService` (`auth.NewIAMServiceSingle`)
holding only the root account. `cmd/gateway` is composition: load `Config`
from the environment, construct both, run `embedgw.RunVersityGW`.

A FoundationDB-backed `auth.IAMService`, so that confidential IAM material
never needed to live outside FDB, was considered and dropped rather than
superseded by a different design: storage location was never a requirement
for IAM, and the built-in Vault backend needs no code beyond configuration.
Consequences worth knowing, not a defect list:

- The gateway process holds plaintext secrets. It reads each account's secret
  from Vault and derives the SigV4 signing key locally. `versitygw`'s
  standalone IAM service keeps secrets out of the gateway process, but it is
  a second process and disables user management through the gateway's own
  admin API, so it is not used.
- Vault holds the whole account record — access key, secret, role (`user`,
  `admin`, `userplus`), user/group/project ID — one KV-v2 entry per access
  key. It does not hold access-control state: bucket ACLs, ownership, and
  bucket policies are backend data, stored by `backend/posix` as extended
  attributes on each bucket's own directory, so they live on the bucket's own
  volume, not in Vault. The root account is not in Vault either; it stays in
  `ROOT_ACCESS_KEY_ID`/`ROOT_SECRET_ACCESS_KEY`.
- Each gateway replica caches accounts independently (`IAMCacheTTL`,
  `IAMCachePrune`). A changed or deleted account takes effect on other
  replicas only after the cache TTL, so a revoked key keeps working for up to
  that long.
- Open: Vault sits outside the bucket volumes' consistency group. After a
  failover, a bucket's ACL owner can name an access key the recovered Vault
  does not have. Whether Vault's own storage should join the same DR policy
  is undecided.
- OpenBao works as a drop-in replacement for HashiCorp Vault; verified
  against OpenBao 2.2.0 and 2.7.1 with AppRole auth, including `create-user`,
  `list-users`, `update-user`, and `delete-user` through the admin API. The
  AppRole policy needs `list` on `secret/metadata/s3` itself for
  `list-users`, not only on the paths below it.

## Configuration

Read from the environment (`internal/config`), the mechanism
`ObjectStore.spec.envFrom` passes through from the operator's gateway
Deployment. `ROOT_ACCESS_KEY_ID`/`ROOT_ACCESS_KEY`, `ROOT_SECRET_ACCESS_KEY`/
`ROOT_SECRET_KEY`, and `VGW_PORT` reuse versitygw's own CLI's env var names
(`cmd/versitygw/main.go`); `GATEWAY_ROOT_DIR`, `GATEWAY_BUCKET_LINKS`, and
`GATEWAY_FSYNC` are this project's own, for settings versitygw's CLI has no
flag for at this embedding layer.

| Variable                                        | Required | Default |
|-------------------------------------------------|----------|---------|
| `GATEWAY_ROOT_DIR`                              | yes      | —       |
| `ROOT_ACCESS_KEY_ID` (or `ROOT_ACCESS_KEY`)     | yes      | —       |
| `ROOT_SECRET_ACCESS_KEY` (or `ROOT_SECRET_KEY`) | yes      | —       |
| `VGW_PORT`                                      | no       | `:9000` |
| `GATEWAY_BUCKET_LINKS`                          | no       | `true`  |
| `GATEWAY_FSYNC`                                 | no       | `true`  |
| `VGW_IAM_CACHE_TTL` (seconds)                   | no       | `120`   |
| `VGW_IAM_CACHE_PRUNE` (seconds)                 | no       | `3600`  |
| `VGW_IAM_VAULT_ENDPOINT_URL`                    | no       | —       |
| `GATEWAY_TLS_CERT_FILE`                         | no       | —       |
| `GATEWAY_TLS_KEY_FILE`                          | no       | —       |

The other `VGW_IAM_VAULT_*` variables are versitygw's own
(`cmd/versitygw/main.go`) and pass through unchanged: `NAMESPACE`,
`SECRET_STORAGE_PATH`, `SECRET_STORAGE_NAMESPACE`, `AUTH_METHOD`,
`AUTH_NAMESPACE`, `MOUNT_PATH`, `ROOT_TOKEN`, `ROLE_ID`, `ROLE_SECRET`,
`SERVER_CERT`, `CLIENT_CERT`, and `CLIENT_CERT_KEY`. The three certificate
variables hold PEM contents, not paths. Setting any of them without
`VGW_IAM_VAULT_ENDPOINT_URL` fails startup. Users are managed through
versitygw's admin API on the S3 port, for example
`versitygw admin -a <root> -s <secret> -er <url> create-user -a <access> -s <secret> -r user`.

`GATEWAY_TLS_CERT_FILE` and `GATEWAY_TLS_KEY_FILE` are paths to the serving
certificate and key, set together or not at all. With both set, the S3 API and
the admin API on the same port serve TLS only. The gateway re-reads both files
every 30 seconds and serves a changed pair without a restart
(`internal/certwatch`). The operator sets them to the certificate it mounts at
`/etc/simplyblock/tls`.

## Commands

```bash
make build   # go build -> build/gateway
make test    # go test ./... -race
make fmt
make vet
make lint
make docker-build IMG=...
```
