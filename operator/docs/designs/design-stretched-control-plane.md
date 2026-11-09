# Stretched control plane on Kubernetes

One simplyblock cluster whose control plane runs in two Kubernetes clusters,
one per site, over one FoundationDB. One site leads at a time; the other
stands by, serves reads, and redirects mutating calls to the leader. The
control-plane side of this (lease, service gate, API verdicts, readiness,
takeover) is sbcli's `docs/stretched-control-plane.md`; this document is the
operator's part: how the two ControlPlanes render one database, how the two
StorageClusters become one backend cluster, and what an administrator does by
hand.

## 1. Shape

```
site A (Kubernetes cluster A)              site B (Kubernetes cluster B)
┌──────────────────────────────┐           ┌──────────────────────────────┐
│ ControlPlane (local)         │           │ ControlPlane (local)         │
│   stretch: site A, peer B    │           │   stretch: site B, peer A    │
│   role Bootstrap             │           │   role Join (seed from A)    │
│ FoundationDBCluster  dc=A ───┼── one DB ─┼── FoundationDBCluster  dc=B  │
│   region A (prio 1), triple  │           │   region B (prio 0), triple  │
│ management API, pools, admin │           │ management API, pools, admin │
│   SB_CONTROL_PLANE_SITE=A    │           │   SB_CONTROL_PLANE_SITE=B    │
│ StorageCluster  stretched:   │           │ StorageCluster  stretched:   │
│   site A, peer B, Leader     │           │   site B, peer A, Follower   │
└──────────────────────────────┘           └──────────────────────────────┘
```

Each Kubernetes cluster runs the operator as a standalone installation. The
two ControlPlane objects describe the same database from each side; the two
StorageCluster objects, with the same name, describe one backend cluster, as
the sync-replication stretched flow already does
(`design-stretched-sync-replication-ramen.md` §5), except that no hub has to
deliver anything: both operators read the same FoundationDB.

## 2. ControlPlane.spec.source.local.stretch

| Field | Meaning |
| --- | --- |
| `site`, `peerSite` | This site's name and the other's. Every control-plane pod learns `site` through `SB_CONTROL_PLANE_SITE`; nodes added from this cluster carry it. |
| `role` | `Bootstrap` creates the database; `Join` joins it. |
| `apiEndpoint` | This site's management API as the other site reaches it (load balancer or node port). The other site's standby redirects mutating calls to it; recorded on the cluster at create (Bootstrap) or registration (Join). |
| `seedConnectionStringSecretRef` | Join only: a Secret in the operator's namespace whose `cluster-file` key is the Bootstrap site's connection string. |

The block is immutable, `site != peerSite`, a Join site needs the seed Secret
(CEL rules on the type).

### 2.1 What the FoundationDBCluster becomes

Both sites render the same `databaseConfiguration`, which the FoundationDB
operator requires of clusters sharing a database:

```yaml
redundancy_mode: triple
usable_regions: 2
regions:
  - datacenters: [{id: <bootstrap site>, priority: 1}]
  - datacenters: [{id: <join site>,      priority: 0}]
```

and each site's own cluster sets `dataCenter: <site>` and
`processGroupIDPrefix: sb-<site>` (the FoundationDB operator wants at least
two characters there); the Join site adds
`seedConnectionString`. Each site runs at least three storage and three log
processes (more where `foundationDB.replicas` asks for more), because triple
needs three zones per region. The fault domain inside a site stays the
hostname, as on a single-site control plane.

This is the FoundationDB operator's multi-region layout (one Kubernetes
cluster per data center), not its multi-Kubernetes layout (each Kubernetes
cluster one zone of a single data center): the latter cannot keep three copies
per site.

### 2.2 Bringing the second site up

1. Install the operator and the ControlPlane on site A with `role: Bootstrap`.
   Until the other site's processes are seen, a site renders the database
   with one usable region and the other at priority -1 (FoundationDB's rule
   for adding a region: processes that join under it take no data), which
   wants five coordinators on five hosts of that site. The operator reads
   the data centers present from its own management API
   (`GET /api/v2/_meta/fdb-datacenters`) every steady-state pass and records
   `status.stretch.peerJoined` once the peer's `dcid` appears; from then on
   both sites render both regions usable (B at priority 0, nine coordinators,
   five on one site and four on the other), and the FoundationDB operator
   walks the change in the steps FoundationDB allows. The record is never
   taken back: a peer that vanishes later is a region outage.
2. Copy the connection string: the `cluster-file` key of the
   `simplyblock-fdb-cluster-config` ConfigMap on site A, into a Secret on site
   B (`kubectl create secret generic fdb-seed --from-file=cluster-file=...`).
3. Install the operator and the ControlPlane on site B with `role: Join` and
   `seedConnectionStringSecretRef: {name: fdb-seed}`. Its FoundationDB
   operator joins the database and recruits region B; the install's
   BuildingIndices step is skipped on a Join site (the indices are the
   database's, already built). Its management API starts as the standby.
4. Apply the StorageCluster on site A (`stretched: {site: A, peerSite: B,
   role: Leader}`), then on site B (`role: Follower`).

Pod IPs of both clusters must route to each other (the processes talk
directly), and both operators must see each other's coordinators in the
connection string. Coordinators are chosen by the FoundationDB operators from
the processes of both sites; a change of coordinators on one side is written
into both sides' cluster files by FoundationDB itself, through the connection
the processes hold. The seed Secret is read at install only.

### 2.3 Cross-site prerequisites the bed found

- **Coordinators by IP.** Each Kubernetes cluster resolves only its own
  Services, so a stretched site renders `routing.useDNSInClusterFile: false`:
  the cluster file names pod IPs, which the shared VPC routes. With DNS names
  the Join site's libfdb_c cannot reach a coordinator and the FoundationDB
  operator crashes in `fdb_run_network` ("Error determining public address").
- **One CA for both sites.** FoundationDB's processes verify each other's
  peer certificates against the CA the chart mints, one per site by default.
  The Join site copies the Bootstrap site's `simplyblock-certificate-authority-tls`
  (cert-manager namespace) under another name and installs with
  `tls.cert-manager.caSecretName` naming it, so every certificate of both
  sites chains to one CA. FoundationDB checks the chain only, not the
  hostname, so IP coordinators verify.

- **An https endpoint the certificate names.** With TLS on (the chart's
  default), each site's `apiEndpoint` is `https://` and the operator adds its
  host to the management API's serving certificate. The other site's
  operator follows the standby's 307 there and verifies it against the shared
  CA; a plain `http://` endpoint ends in EOF against the TLS listener.
- **One StorageCluster name.** The follower finds the backend cluster by
  its StorageCluster's name, so both sites must use the same one. A
  discovery derives a different name on each site (it is disambiguated by the
  Kubernetes cluster's identity), so the follower's draft is given the
  leader's name (`spec.cluster.name`) before it is approved; the hub-driven
  flow writes one name to both sites anyway.

## 3. StorageCluster

The existing leader/follower flow is reused unchanged in shape; the
differences are where the credentials come from and what the create carries.

- **Leader (site A).** `creationParams` adds `stretched_control_plane: true`,
  `site: A` and `site_api_endpoint` from the ControlPlane, beside
  `leader_site: A`. A StorageCluster that does not name the control plane's
  site, or names another, is refused with `StretchedSiteMismatch` before any
  POST: the cluster the create would make could not be adopted by the other
  side.
- **Follower (site B).** `awaitLeader` finds the leader's cluster by name in
  its own control plane's list, secret included, runs the same preflight as
  the hub path (sync cluster, leader site is the other one, no foreign node
  on this site, workers reachable), and adopts. Before persisting it
  registers this site's control plane with the cluster
  (`POST /management-nodes` with `site` and `api_endpoint`), once: a site the
  cluster already lists, or a 409, is a registration an earlier pass made.
  The registration is what fills `Cluster.site_api_endpoints[B]`, the address
  the leader's standby redirects to once B holds the lease.

Every other call the follower's operator makes (node add, pool, service
classes) goes to its own, standby, control plane and is answered with a 307
to the leader's `apiEndpoint`. Both HTTP clients of the operator
(`internal/webapi` and atlas-lib's) follow it with the bearer token put back,
which Go otherwise strips on a cross-host redirect, and with the body resent
(307 keeps method and body).

## 4. Readiness, fencing and the site's identity

Nothing here changes: the control plane decides who leads (sbcli lease
agent), fences by pausing its own services, and the operator on the standby
side keeps reconciling against its local API, which serves reads and redirects
writes. The operator never moves the lease; `control-plane set-leader-site`
and `takeover` are an administrator's or an upper layer's calls.

## 5. Supported data-plane schemes on Kubernetes

Scheme (a) of the design, any n+k with synchronous replication across the two
sites, is what the StorageCluster side supports today: the StorageCluster CEL
still requires `enableSyncReplication` with `stretched`. Schemes (b) and (c),
1+1 and 1+2 with the failure domain bound to the site and no sync
replication, are implemented in the control plane (`sn add-node --site`
derives the failure domain, activation needs `npcs+1` sites) and need the
StorageCluster rule relaxed and the follower preflight taught that a
stretched-control-plane cluster without sync replication is legal. Tracked as
follow-up.

## 6. Test ladder

| Level | What | Status |
| --- | --- | --- |
| 1 | sbcli unit and integration tests, operator unit tests | green |
| 2 | Two Docker Swarm sites on AWS, 1+1 sync replication, lease move, takeover, red gate (`sbcli scripts/aws_stretched_cp_level2.py`) | 20/20, 2026-11-09 |
| 3 | Two Kubernetes clusters in one VPC, this document's layout, F-scenarios, latency, recovery rehearsal | next |
| 4 | Metro-DR over the stretched cluster (Ramen, k3s hub) | after 3 |

## 7. Limits and open points

- **Two sites only.** Coordinators split 5/4 across the sites; losing the
  site with five is an unavailable database until the coordination state is
  recovered by hand (`fdbcli` forced recovery), which is the design's stated
  trade-off without a third site.
- **Connection string drift.** The seed is read once. If every coordinator of
  one site is replaced while the other site is down, the returning site's
  cluster file is stale; the FoundationDB operator's `kubectl fdb recover
  multi-region` is the repair.
- **TLS.** A site's `apiEndpoint` is dialed by the other site's operator and
  by clients following redirects; with `tls` enabled it has to present a
  certificate those trust. The level-3 bed runs plaintext inside one VPC.
- **One ControlPlane per Kubernetes cluster.** The stretch block is on the
  singleton; a Kubernetes cluster is one site.
