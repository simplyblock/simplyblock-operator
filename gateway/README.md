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
`docs/adr/0002-…`, `notes/versitygw-put-durability.md`). `embedgw.Config`
already falls back to a single-account `auth.IAMService`
(`auth.NewIAMServiceSingle`) when no IAM backend is configured — exactly the
stub this project needs until a real FDB- or Vault-backed IAM design is
decided (`notes/versitygw-iam-fdb-secret-storage.md`; `embedgw.Config` already
carries `VaultEndpointURL` and the rest of a Vault-backed IAM's config surface
for when that lands). `cmd/gateway` is composition: load `Config` from the
environment, construct both, run `embedgw.RunVersityGW`.

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

## Commands

```bash
make build   # go build -> build/gateway
make test    # go test ./... -race
make fmt
make vet
make lint
make docker-build IMG=...
```
