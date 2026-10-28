# gateway

The S3-compatible gateway (`simplyblock-s3-interface-spec.pdf`, Phase 1): an
embedded [versitygw](https://github.com/versity/versitygw)
(`github.com/versity/versitygw/embedgw`) over its own `backend/posix`, serving
one bucket per simplyblock volume via `--bucketlinks`
(`docs/adr/0002-bucket-per-volume-via-bucketlinks.md`). The `ObjectStore`
reconciler that deploys this as a Kubernetes workload
(`operator/docs/designs/design-objectstore.md`) does not exist yet; this
component is standalone, buildable and testable on its own, until that lands.

## Why no backend or IAM code lives here

`github.com/versity/versitygw/backend/posix` already implements
`backend.Backend` as a complete POSIX filesystem backend, bucket-per-volume
symlinks and durable `PutObject` included (`--bucketlinks`, `--fsync`:
`docs/adr/0002-…`, `notes/versitygw-put-durability.md`). IAM is versitygw's
built-in Vault backend, used as-is (`docs/adr/0007-gateway-iam-is-versitygw-vault-backend.md`),
configured through versitygw's own `VGW_IAM_VAULT_*` variables. Without
`VGW_IAM_VAULT_ENDPOINT_URL`, `embedgw` falls back to a single-account
`auth.IAMService` (`auth.NewIAMServiceSingle`) holding only the root account.
`cmd/gateway` is composition: load `Config` from the environment, construct
both, run `embedgw.RunVersityGW`.

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
