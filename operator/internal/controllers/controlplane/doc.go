// Package controlplane reconciles the ControlPlane singleton and the operations
// performed against it.
//
// The kind says one of two things. spec.source.local is a control plane this
// cluster hosts, which the operator installs and owns. spec.source.managed is a
// control plane elsewhere that this cluster's storage is managed by, which the
// operator only resolves and probes.
//
// The kind is the root of the ownership spine: a StorageCluster cannot be
// created, a StorageNode cannot be added, and a volume cannot be provisioned
// until it reports Available. That makes two things this package does more
// load-bearing than they look. It installs, which is the work the Helm chart
// used to do and which every deployment now depends on. And it publishes
// status.endpoint, which is where the rest of the operator learns how to reach
// the control plane.
//
// # What the install applies
//
// The managed install applies the objects a base control plane consists of,
// which is what the chart rendered with observability disabled:
//
//   - The FoundationDB operator, its RBAC, and the FoundationDBCluster itself.
//     The operator half is skipped where the Kubernetes cluster already serves
//     apps.foundationdb.org, since a second one reconciling the same objects is
//     worse than depending on the first.
//   - The object store, which is MinIO and the bucket configuration beside it.
//   - The management API, the services beside it, their shared account, the
//     configuration they read, and the Service the endpoint resolves to.
//
// What it does not apply is the observability half: Graylog, Grafana, Thanos,
// the document store behind them, and the log collector. Those are gated behind
// one chart value, none of them appears in a step of the installation machine,
// and every one is non-essential in the phase table, so they stay where they
// are. [componentTable] lists what is installed and therefore watched.
//
// # Why the datastore step applies an object store
//
// design-controlplane.md §4.2 names ApplyingDatastore "the document store the
// management API needs," and §5.1 identifies that as a MongoDBCommunity. A base
// deployment has none: the chart renders the MongoDBCommunity only with
// observability enabled, because what stores documents in it is Graylog rather
// than the management API. The step therefore applies the store a base
// deployment does have, which is MinIO. §12 Q6 asks what supplies the MongoDB
// operator where a cluster has none, and the answer this package reaches is that
// a base control plane does not need one.
//
// # TLS
//
// spec.source.local.tls is what the install reads, and it defaults to serving
// TLS and requiring a client certificate. tls.go turns it into the environment
// the control-plane image has always spoken -- SB_TLS_SERVE, SB_TLS_PROVIDER,
// SB_TLS_CONNECT, SB_TLS_CLIENT_AUTH, and FoundationDB's FDB_TLS_* -- onto every
// pod of the install rather than only the one that serves, because the task and
// monitoring pools are clients of the management API and a plaintext pool cannot
// reach an API that requires a certificate.
//
// The serving certificate is applied here as well. It was the chart's, rendered
// beside the Service it certifies, and the install that replaced the chart took
// the Service without it, so every pod mounted a Secret nothing produced.
//
// # What is not here yet
//
// Adoption. An install that meets objects a Helm release already created takes
// them over by server-side apply under a stable field manager, which is the same
// mechanism the CSI driver's adoption uses, but nothing here verifies the
// handover or strips the release's claim afterward. design-controlplane.md §12
// Q2 is the open question, and taking over a live control plane is the half of
// it this package does not yet answer.
package controlplane
