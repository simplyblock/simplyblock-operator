const LAYER_META = {
  clusters: {label: "Clusters", icon: "cluster"}, cluster: {icon: "cluster"},
  cp: {label: "Control plane", icon: "host"},
  hosts: {label: "Hosts", icon: "host"}, host: {icon: "host"},
  nodes: {label: "Storage nodes", icon: "node"}, node: {icon: "node"},
  devices: {label: "Devices", icon: "device"}, device: {icon: "device"},
  pools: {label: "Storage pools", icon: "pool"}, pool: {icon: "pool"},
  volumes: {label: "Logical volumes", icon: "volume"}, volume: {icon: "volume"},
  snapshots: {label: "Snapshots", icon: "camera"}, snapshot: {icon: "camera"},
  backups: {label: "Backups", icon: "cloud"}, backup: {icon: "cloud"},
  policies: {label: "Backup policies", icon: "clock"}, policy: {icon: "clock"},
  dr: {label: "Disaster recovery", icon: "shield"},
  plans: {label: "Protection plans", icon: "shield"}, plan: {icon: "shield"},
  sites: {label: "Sites", icon: "k8s"}, site: {icon: "k8s"},
  slots: {label: "Replication slots", icon: "volume"}, slot: {icon: "volume"},
  replops: {label: "Operations", icon: "clock"}, replop: {icon: "clock"},
  pairs: {label: "Replication pairs", icon: "swap"}, pair: {icon: "swap"},
  rpolicies: {label: "Replication policies", icon: "shield"}, rpolicy: {icon: "shield"},
  zones: {label: "Zones", icon: "zone"}, zone: {icon: "zone"},
  cgroups: {label: "Consistency groups", icon: "link"}, cgroup: {icon: "link"},
  cgsnapshots: {label: "Group snapshots", icon: "camera"}, cgsnapshot: {icon: "camera"},
  migrations: {label: "Migrations", icon: "move"}, migration: {icon: "move"},
  k8s: {label: "Kubernetes", icon: "k8s"}, k8sc: {icon: "k8s"},
  discovery: {label: "Discovery & deployment", icon: "search"},
  deploywizard: {label: "Deploy a cluster", icon: "plus"},
  deployconfigs: {label: "Deployment documents", icon: "cluster"}, deployconfig: {icon: "cluster"},
  storageclasses: {label: "Storage classes", icon: "pool"}, storageclass: {icon: "pool"},
  pvcs: {label: "PVCs", icon: "volume"}, pvc: {icon: "volume"},
  buckets: {label: "Buckets", icon: "cloud"}, bucket: {icon: "cloud"},
  protectedapps: {label: "Protected applications", icon: "cluster"}, protectedapp: {icon: "cluster"},
  mpaths: {label: "Migration paths", icon: "move"}, mpath: {icon: "move"},
  appgroups: {label: "Application groups", icon: "cluster"}, appgroup: {icon: "cluster"}
};
const pC = cid => [{t: "clusters"}, {t: "cluster", id: cid}];
const pH = (cid, hid) => [...pC(cid), {t: "hosts"}, {t: "host", id: hid}];
const pN = (cid, nid) => [...pC(cid), {t: "nodes"}, {t: "node", id: nid}];
const pD = (cid, nid, did) => [...pN(cid, nid), {t: "devices"}, {t: "device", id: did}];
const pP = (cid, pid) => [...pC(cid), {t: "pools"}, {t: "pool", id: pid}];
const pV = (cid, pid, vid) => [...pP(cid, pid), {t: "volumes"}, {t: "volume", id: vid}];
const pPlan = id => [{t: "dr"}, {t: "plans"}, {t: "plan", id}];
const pSite = id => [{t: "dr"}, {t: "sites"}, {t: "site", id}];
const pPair = id => [{t: "dr"}, {t: "pairs"}, {t: "pair", id}];
const pSlot = id => [{t: "dr"}, {t: "slots"}, {t: "slot", id}];
const pReplOp = id => [{t: "dr"}, {t: "replops"}, {t: "replop", id}];
const pRPol = id => [{t: "dr"}, {t: "rpolicies"}, {t: "rpolicy", id}];
const pZone = id => [{t: "dr"}, {t: "zones"}, {t: "zone", id}];
const pCg = (cid, gid) => [...pC(cid), {t: "cgroups"}, {t: "cgroup", id: gid}];
const pMig = (cid, id) => [...pC(cid), {t: "migrations"}, {t: "migration", id}];
const pBucket = (cid, id) => [...pC(cid), {t: "buckets"}, {t: "bucket", id}];
const pK = id => [{t: "k8s"}, {t: "k8sc", id}];
const pSc = (kid, id) => [...pK(kid), {t: "storageclasses"}, {t: "storageclass", id}];
const pDep = (kid, id) => [...pK(kid), {t: "deployconfigs"}, {t: "deployconfig", id}];
const pPvc = (kid, id) => [...pK(kid), {t: "pvcs"}, {t: "pvc", id}];
const pApp = id => [{t: "dr"}, {t: "protectedapps"}, {t: "protectedapp", id}];
const pMp = id => [{t: "dr"}, {t: "mpaths"}, {t: "mpath", id}];
const pAg = (pid, id) => [...pMp(pid), {t: "appgroups"}, {t: "appgroup", id}];
const detailPath = o => o.kind === "cluster" ? pC(o.id)
  : o.kind === "deployconfig" ? pDep(o.k8sClusterId, o.id)
  : o.kind === "host" ? pH(o.clusterId, o.id)
  : o.kind === "node" ? pN(o.clusterId, o.id)
  : o.kind === "device" ? pD(o.clusterId, o.nodeId, o.id)
  : o.kind === "pool" ? pP(o.clusterId, o.id)
  : o.kind === "volume" ? pV(o.clusterId, o.poolId, o.id)
  : o.kind === "plan" ? pPlan(o.id)
  : o.kind === "site" ? pSite(o.id)
  : o.kind === "pair" ? pPair(o.id)
  : o.kind === "slot" ? pSlot(o.id)
  : o.kind === "replops" ? pReplOp(o.id)
  : o.kind === "rpolicy" ? pRPol(o.id)
  : o.kind === "zone" ? pZone(o.id)
  : o.kind === "protectedapp" ? pApp(o.id)
  : o.kind === "mpath" ? pMp(o.id)
  : o.kind === "appgroup" ? pAg(o.pathId, o.id)
  : o.kind === "bucket" ? pBucket(o.clusterId, o.id)
  : o.kind === "k8sc" ? pK(o.id)
  : o.kind === "storageclass" ? pSc(o.k8sClusterId, o.id)
  : o.kind === "pvc" ? pPvc(o.k8sClusterId, o.id)
  : o.kind === "migration" ? pMig(o.sourceClusterId || o.clusterId, o.id)
  : o.kind === "cgroup" ? pCg(o.clusterId, o.id)
  : o.kind === "cgsnapshot" ? [...pCg(o.clusterId, o.cgId), {t: "cgsnapshots"}, {t: "cgsnapshot", id: o.id}]
  : o.kind === "snapshot" ? [...pV(o.clusterId, o.poolId, o.volumeId), {t: "snapshots"}, {t: "snapshot", id: o.id}]
  : o.kind === "backup" ? [...pV(o.clusterId, o.poolId, o.volumeId), {t: "backups"}, {t: "backup", id: o.id}]
  : [...pC(o.clusterId), {t: "policies"}, {t: "policy", id: o.id}];

const nameOf = o => o.hostname || o.name || (o.mode === "nvme" ? o.serial : o.blockdev) || o.serial || o.chainId || "";
const segLabel = s => s.id ? (REG[s.id] ? nameOf(REG[s.id]) : shortId(s.id)) : LAYER_META[s.t].label;
const searchable = o => [nameOf(o), o.id, o.ip, o.mgmtIp, o.serial, o.pcie, o.blockdev, o.failureDomain,
  o.owner, o.storageClass, ...Object.entries(o.tags || {}).map(([k, v]) => `${k}=${v}`),
  o.physicalLabel, o.poolName, o.volumeName, o.chainId, o.bucket, o.type, o.finest,
  o.rack, o.cabinet, o.hostClass, o.location, o.region, o.mode,
  o.replication && o.replication.policyName,
  o.sourceClusterId && regName(o.sourceClusterId), o.targetClusterId && regName(o.targetClusterId)
].filter(Boolean).join(" ").toLowerCase();
const healthOf = o => (STATUS_META[o.status] || {rank: 9}).rank;
const ioSum = o => ((o.iops || {}).r || 0) + ((o.iops || {}).w || 0);
const bwSum = o => ((o.bw || {}).r || 0) + ((o.bw || {}).w || 0);
const ts = o => Date.parse(o.createdAt || 0) || 0;

const SORTS = {
  health: {label: "Unhealthy first", cmp: (a, b) => healthOf(b) - healthOf(a) || nameOf(a).localeCompare(nameOf(b))},
  name: {label: "Name (A–Z)", cmp: (a, b) => nameOf(a).localeCompare(nameOf(b))},
  util: {label: "Utilization ↓", cmp: (a, b) => pct(b.capacity.used, b.capacity.total) - pct(a.capacity.used, a.capacity.total)},
  cap: {label: "Capacity ↓", cmp: (a, b) => b.capacity.total - a.capacity.total},
  iops: {label: "IOPS ↓", cmp: (a, b) => ioSum(b) - ioSum(a)},
  bw: {label: "Throughput ↓", cmp: (a, b) => bwSum(b) - bwSum(a)},
  newest: {label: "Newest first", cmp: (a, b) => ts(b) - ts(a)},
  oldest: {label: "Oldest first", cmp: (a, b) => ts(a) - ts(b)},
  size: {label: "Size ↓", cmp: (a, b) => b.capacity.total - a.capacity.total},
  free: {label: "Free devices ↓", cmp: (a, b) => (b.counts.free || 0) - (a.counts.free || 0)},
  backlog: {label: "Backlog ↓", cmp: (a, b) => (b.backlog || 0) - (a.backlog || 0)},
  members: {label: "Volumes ↓", cmp: (a, b) => (b.counts.volumes || 0) - (a.counts.volumes || 0)},
  slots: {label: "Slots ↓", cmp: (a, b) => (b.counts.slots || 0) - (a.counts.slots || 0)},
  hosts: {label: "Hosts ↓", cmp: (a, b) => (b.counts.hosts || 0) - (a.counts.hosts || 0)},
  rtt: {label: "Latency ↑", cmp: (a, b) => (a.link ? a.link.rtt_ms : 0) - (b.link ? b.link.rtt_ms : 0)}
};
const SORT_KEYS = {
  cluster: ["health", "name", "util", "cap", "iops", "bw"],
  host: ["health", "free", "name", "cap"],
  node: ["health", "name", "util", "cap", "iops", "bw"],
  device: ["health", "name", "util", "cap", "iops", "bw"],
  pool: ["health", "name", "util", "cap"],
  volume: ["health", "name", "util", "cap", "iops", "bw", "newest"],
  snapshot: ["newest", "oldest", "size", "name"],
  backup: ["newest", "oldest", "size", "name", "health"],
  policy: ["name", "members"],
  plan: ["health", "name", "members", "newest"],
  site: ["health", "name", "members"],
  pair: ["health", "name", "slots", "newest"],
  slot: ["health", "name", "newest"],
  replops: ["newest", "health", "name"],
  rpolicy: ["health", "slots", "name", "newest"],
  zone: ["name", "hosts", "cap"],
  cgroup: ["health", "name", "members", "cap"],
  cgsnapshot: ["newest", "oldest", "size", "name"],
  migration: ["health", "name", "newest", "members"],
  k8sc: ["health", "name", "members"],
  storageclass: ["name", "members", "cap"],
  pvc: ["health", "name", "cap", "newest"],
  bucket: ["health", "name", "util", "cap", "newest"],
  protectedapp: ["health", "name", "newest"]
};

const clusterOf = seg => seg.t === "cluster" ? seg.id : (REG[seg.id] || {}).clusterId;
// Each view advertises the surface that actually backs it: a CRD, a core object,
// or an operator /proposed collection for the kinds v1alpha1 does not model yet.
const G_ = "/apis/storage.simplyblock.io/v1alpha1/namespaces/{ns}";
const crd = (p, sel) => `GET ${G_}/${p}${sel ? "?labelSelector=" + sel.replace("storage.simplyblock.io/", "…/") : ""}`;
const crd1 = p => `GET ${G_}/${p}/{name}`;
const prop = p => `GET /operator/v1/proposed/${p}`;
const OWNER = "storage.simplyblock.io/owner-name";
const CLUS = "storage.simplyblock.io/cluster";
const PV = "/api/v1/persistentvolumes";
const VIEWS = {
  hosts: {kind: "host",
    load: p => p.t === "zone" ? api.zoneHosts(p.id) : p.t === "k8sc" ? api.k8sHosts(p.id) : api.hosts(p.id),
    api: p => prop(`hosts?scope=${p.t === "zone" ? "zones" : p.t === "k8sc" ? "k8s-clusters" : "clusters"}`)},
  nodes: {kind: "node",
    load: p => p.t === "host" ? api.nodes(clusterOf(p)).then(ns => ns.filter(n => n.hostId === p.id)) : api.nodes(p.id),
    api: p => crd("storagenodes", p.t === "host" ? OWNER : CLUS)},
  devices: {kind: "device", load: p => api.devices(p.id), api: () => crd("storagedevices", OWNER)},
  pools: {kind: "pool", load: p => api.pools(p.id), api: () => crd("storagepools", CLUS)},
  volumes: {kind: "volume",
    load: p => p.t === "rpolicy" ? api.rpolicyVolumes(p.id) : p.t === "cgroup" ? api.cgroupVolumes(p.id)
      : p.t === "migration" ? api.migrationVolumes(p.id) : p.t === "appgroup" ? api.appGroupVolumes(p.id)
      : p.t === "pool" ? api.poolVolumes(p.id) : api.clusterVolumes(p.id),
    api: p => p.t === "appgroup" ? prop("app-groups/{uuid}/lvols") : ["rpolicy", "cgroup", "migration"].includes(p.t) ? prop(`${p.t}s/{uuid}/lvols`)
      : `GET ${PV}?labelSelector=storage.simplyblock.io/${p.t === "pool" ? "pool" : "cluster"}`},
  cgroups: {kind: "cgroup", load: p => api.cgroups(p.id), api: () => prop("consistency-groups?cluster={uuid}")},
  k8s: {kind: "k8sc", load: () => api.k8sClusters(), api: () => prop("kubernetes-clusters")},
  deployconfigs: {kind: "deployconfig",
    load: p => p.t === "k8sc" ? api.k8sDeployConfigs(p.id) : api.deployConfigs(),
    api: () => crd("clusterdeploymentconfigs")},
  mpaths: {kind: "mpath", load: () => api.mpaths(), api: () => prop("migration-paths")},
  appgroups: {kind: "appgroup", load: p => api.mpathGroups(p.id), api: () => prop("migration-paths/{uuid}/app-groups")},
  protectedapps: {kind: "protectedapp",
    load: p => p.t === "plan" ? api.planApps(p.id) : p.t === "site" ? api.siteApps(p.id)
      : p.t === "rpolicy" ? api.rpolicyApps(p.id) : api.protectedApps(),
    api: p => prop(p.t === "plan" ? "protection-plans/{uuid}/protected-apps"
      : p.t === "site" ? "dr-sites/{uuid}/protected-apps"
      : p.t === "rpolicy" ? "replication-policies/{uuid}/protected-apps" : "protected-apps")},
  storageclasses: {kind: "storageclass",
    load: p => p.t === "pool" ? api.poolStorageClasses(p.id) : api.k8sStorageClasses(p.id),
    api: () => "GET /apis/storage.k8s.io/v1/storageclasses"},
  pvcs: {kind: "pvc",
    load: p => p.t === "storageclass" ? api.storageClassPvcs(p.id) : p.t === "k8sc" ? api.k8sPvcs(p.id)
      : p.t === "protectedapp" ? api.protectedAppPvcs(p.id) : api.pvcs(),
    api: () => "GET /api/v1/namespaces/{ns}/persistentvolumeclaims"},
  buckets: {kind: "bucket", load: p => api.buckets(p.id), api: () => prop("buckets?cluster={uuid}")},
  migrations: {kind: "migration",
    load: p => p.t === "cluster" ? api.migrations(p.id) : api.allMigrations(),
    api: p => prop(p.t === "cluster" ? "migrations?cluster={uuid}" : "migrations")},
  cgsnapshots: {kind: "cgsnapshot", load: p => api.cgSnapshots(p.id), api: () => prop("cg-snapshots?group={uuid}")},
  plans: {kind: "plan",
    load: p => p.t === "site" ? api.sitePlans(p.id) : api.plans(),
    api: p => prop(p.t === "site" ? "dr-sites/{uuid}/protection-plans" : "protection-plans")},
  sites: {kind: "site",
    load: p => p.t === "plan" ? api.planSites(p.id) : api.sites(),
    api: p => prop(p.t === "plan" ? "protection-plans/{uuid}/sites" : "dr-sites")},
  // replication reads three CRD lists and joins them: the resources reference
  // each other by name and carry no rollups
  pairs: {kind: "pair", load: () => api.pairs(), api: () => crd("replicationpairs")},
  slots: {kind: "slot",
    load: p => p.t === "rpolicy" ? api.policySlots(p.id) : p.t === "pair" ? api.pairSlots(p.id) : api.slots(),
    api: () => crd("replicationslots")},
  replops: {kind: "replops",
    load: p => (p.t === "rpolicy" || p.t === "pair") ? api.refReplOps((REG[p.id] || {}).name) : api.replOps(),
    api: () => crd("replicationops")},
  rpolicies: {kind: "rpolicy",
    load: p => p.t === "pair" ? api.pairRPolicies(p.id) : p.t === "cluster" ? api.rpolicies(p.id) : api.allRPolicies(),
    api: () => crd("replicationpolicies")},
  zones: {kind: "zone",
    load: p => p.t === "cluster" ? api.clusterZones(p.id) : p.t === "k8sc" ? api.k8sZones(p.id) : api.zones(),
    api: () => prop("zones")},
  snapshots: {kind: "snapshot",
    load: p => api.snapshots(p.t === "volume" ? "lvols" : p.t === "pool" ? "pools" : "clusters", p.id),
    api: () => prop("snapshots?parent={uuid}")},
  backups: {kind: "backup",
    load: p => api.backups(p.t === "volume" ? "lvols" : p.t === "pool" ? "pools" : "clusters", p.id),
    api: () => crd("storagebackups", CLUS)},
  policies: {kind: "policy", load: p => api.policies(p.id), api: () => prop("backup-policies?cluster={uuid}")},
  clusters: {kind: "cluster",
    load: p => p.t === "zone" ? api.zoneClusters(p.id) : p.t === "k8sc" ? api.k8sStorageClusters(p.id) : api.clusters(),
    api: () => crd("storageclusters")}
};
const DETAIL_API = {
  cluster: crd1("storageclusters"), node: crd1("storagenodes"), device: crd1("storagedevices"),
  pool: crd1("storagepools"), backup: crd1("storagebackups"),
  host: "GET /api/v1/nodes/{name}", volume: `GET ${PV}/{name}`,
  storageclass: "GET /apis/storage.k8s.io/v1/storageclasses/{name}",
  pvc: "GET /api/v1/namespaces/{ns}/persistentvolumeclaims/{name}",
  snapshot: prop("snapshots/{uuid}"), policy: prop("backup-policies/{uuid}"),
  plan: prop("protection-plans/{uuid}"), site: prop("dr-sites/{uuid}"),
  pair: crd1("replicationpairs"), rpolicy: crd1("replicationpolicies"),
  slot: crd1("replicationslots"), replops: crd1("replicationops"),
  zone: prop("zones/{uuid}"), cgroup: prop("consistency-groups/{uuid}"),
  cgsnapshot: prop("cg-snapshots/{uuid}"), migration: prop("migrations/{uuid}"),
  k8sc: prop("kubernetes-clusters/{uuid}"), bucket: prop("buckets/{uuid}"),
  protectedapp: prop("protected-apps/{uuid}"),
  mpath: prop("migration-paths/{uuid}"), appgroup: prop("app-groups/{uuid}"),
  deployconfig: crd1("clusterdeploymentconfigs")};

const KIND_LABEL = {cluster: "cluster", host: "host", node: "storage node", device: "device", pool: "storage pool",
  volume: "logical volume", snapshot: "snapshot", backup: "backup", policy: "backup policy",
  plan: "protection plan", site: "site",
  pair: "replication pair", rpolicy: "replication policy",
  slot: "replication slot", replops: "replication operation", zone: "zone",
  cgroup: "consistency group", cgsnapshot: "group snapshot", migration: "migration",
  k8sc: "Kubernetes cluster", storageclass: "storage class", pvc: "persistent volume claim", bucket: "bucket",
  protectedapp: "protected application",
  deployconfig: "deployment document", mpath: "migration path", appgroup: "application group"};

function ErrorState({error, onRetry, kind, onUp, upLabel}) {
  if (error.status === 501) {
    return (
      <div className="empty">
        <Icon n="link" s={24} c="var(--warn)" />
        <b style={{color: "var(--text)"}}>Not available on this cluster</b>
        <span style={{maxWidth: 480}}>{error.message}</span>
        <span style={{maxWidth: 480, color: "var(--dim2)"}}>Snapshot replication itself still works here — it is the orchestration of asynchronous replication that is unavailable on this cluster.</span>
        {onUp && <button className="chip" style={{marginTop: 8}} onClick={onUp}>Back to {upLabel}</button>}
      </div>
    );
  }
  if (error.status === 404) {
    return (
      <div className="empty">
        <Icon n="alert" s={24} c="var(--dim2)" />
        <b style={{color: "var(--text)"}}>This {KIND_LABEL[kind] || "object"} no longer exists</b>
        <span style={{maxWidth: 440}}>It was removed from the control plane, or this link points at an object from an earlier deployment.</span>
        <span className="mono" style={{fontSize: 10.5}}>404 · {API}{error.path}</span>
        {onUp && <button className="chip" style={{marginTop: 8}} onClick={onUp}>Back to {upLabel}</button>}
      </div>
    );
  }
  return (
    <div className="empty">
      <Icon n="alert" s={24} c="var(--bad)" />
      <b style={{color: "var(--text)"}}>Could not reach the Kubernetes API</b>
      <span style={{maxWidth: 420}}>{error.status ? `HTTP ${error.status} — ` : ""}{error.message}</span>
      <span className="mono" style={{fontSize: 10.5}}>{(error.path || "").startsWith("/apis") || (error.path || "").startsWith("/api/") ? API : window.SB_CONFIG.operatorBase}{error.path}</span>
      <button className="chip" style={{marginTop: 8}} onClick={onRetry}><Icon n="refresh" s={12} />Retry</button>
    </div>
  );
}

function Toolbar({items, kind, scope, q, setQ, sort, setSort, filters, setFilters, density, setDensity, count, onRefresh, extra}) {
  const counts = {};
  items.forEach(o => counts[o.status] = (counts[o.status] || 0) + 1);
  const order = Object.keys(counts).sort((a, b) => (STATUS_META[b] || {rank: 0}).rank - (STATUS_META[a] || {rank: 0}).rank);
  const keys = SORT_KEYS[kind] || ["name"];
  return (
    <div className="toolbar">
      <div className="search"><Icon n="search" s={13} c="var(--dim2)" />
        <input value={q} placeholder="Search name, UUID, IP, serial…  ( / )" onChange={e => setQ(e.target.value)} />
        {q && <button onClick={() => setQ("")} style={{display: "flex", color: "var(--dim2)"}}><Icon n="x" s={11} /></button>}
      </div>
      {scope && <span className="scopepill" title="Results are pre-filtered to this scope"><Icon n={LAYER_META[scope.t].icon} s={11} />{scope.label}</span>}
      <div className="chips">
        {order.length > 1 && order.map(s => (
          <button key={s} className={"chip" + (filters.includes(s) ? " on" : "")} onClick={() => setFilters(filters.includes(s) ? filters.filter(x => x !== s) : [...filters, s])}>
            <Dot c={(STATUS_META[s] || {}).c} />{(STATUS_META[s] || {}).label || s}<span className="n">{counts[s]}</span>
          </button>
        ))}
        {filters.length > 0 && <button className="chip" onClick={() => setFilters([])}><Icon n="x" s={10} />clear</button>}
      </div>
      <div className="spacer"></div>
      <span className="count">{count === items.length ? items.length : `${count} / ${items.length}`}</span>
      <select className="sel" value={sort} onChange={e => setSort(e.target.value)} title="Sort tiles">
        {keys.map(k => <option key={k} value={k}>{SORTS[k].label}</option>)}
      </select>
      <div className="seg">
        {[["380px", "S"], ["340px", "M"], ["290px", "L"]].map(([v, l], i) => (
          <button key={v} className={density === v ? "on" : ""} title={["Spacious", "Balanced", "Compact"][i]} onClick={() => setDensity(v)}>{l}</button>
        ))}
      </div>
      <button className="chip" onClick={onRefresh} title="Refresh from the Kubernetes API"><Icon n="refresh" s={12} /></button>
      {extra}
    </div>
  );
}

function DiscoveryView({kid, nav}) {
  const {data: k, loading, error, reload} = useResource("dv|" + kid, () => api.k8sCluster(kid));
  if (error) return <div className="scroll"><ErrorState error={error} onRetry={reload} kind="k8sc" /></div>;
  if (loading || !k) return <div className="scroll"><div className="stats">
    {Array.from({length: 4}).map((_, i) => <div className="skel" key={i} style={{height: 62}}></div>)}</div></div>;
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}>
        <h1>Discovery &amp; deployment</h1>
        <div className="mdesc" style={{marginTop: 4}}>The control plane and the operator are installed with Helm, outside this console. From here on the operator does the work: it discovers what hardware <b>{k.name}</b> has, and a deployment document turns that into a storage cluster.</div>
      </div></div>
      <DiscoveryPanel k={k} nav={nav} />
    </div>
  );
}

const TILE = {cluster: ClusterTile, host: HostTile, node: NodeTile, device: DeviceTile, pool: PoolTile,
  volume: VolumeTile, snapshot: SnapshotTile, backup: BackupTile, policy: PolicyTile,
  plan: PlanTile, site: SiteTile, pair: PairTile, rpolicy: RPolicyTile,
  slot: SlotTile, replops: ReplOpsTile, zone: ZoneTile, cgroup: CgroupTile, cgsnapshot: CgSnapshotTile,
  migration: MigrationTile, k8sc: K8sTile, storageclass: StorageClassTile, pvc: PvcTile, bucket: BucketTile,
  protectedapp: ProtectedAppTile,
  deployconfig: DeployConfigTile, mpath: MPathTile, appgroup: AppGroupTile};
const TKEY = {cluster: "c", host: "h", node: "n", device: "d", pool: "p", volume: "v", snapshot: "s",
  backup: "b", plan: "p", site: "s", policy: "p", pair: "p", rpolicy: "p",
  slot: "s", replops: "o", zone: "s", cgroup: "g", cgsnapshot: "s", migration: "m", k8sc: "k", storageclass: "s", pvc: "p", bucket: "b",
  protectedapp: "a", deployconfig: "d", mpath: "m", appgroup: "g"};

const kindPlural = kind => {
  const l = KIND_LABEL[kind] || kind;
  return /(s|ss|y)$/.test(l) && !/[aeiou]y$/.test(l) ? l.replace(/y$/, "ies") : l + "s";
};

function OverviewView({seg, parent, nav, prefs, rev, up, upLabel}) {
  const {q, setQ, sort, setSort, filters, setFilters, density, setDensity} = prefs;
  const [sel, setSel] = useState([]);
  const XF0 = {sc: "", ann: "", tag: "", region: "", feat: ""};
  const [xf, setXf] = useState(XF0);
  useEffect(() => { setXf(XF0); }, [seg.t, parent && parent.id]);
  // S3 tag filter: "key" matches any bucket carrying the key, "key=value" exact
  const tagMatch = (o, t) => {
    if (!t) return true;
    const [k, v] = t.split("=").map(s => s.trim());
    const tags = o.tags || {};
    return v === undefined ? Object.keys(tags).some(x => x.toLowerCase().includes(k.toLowerCase()))
      : Object.entries(tags).some(([x, y]) => x.toLowerCase() === k.toLowerCase() && String(y).toLowerCase() === v.toLowerCase());
  };
  const featMatch = (o, f) => !f || (f === "versioned" ? o.versioning : f === "locked" ? o.objectLock : f === "public" ? o.access && o.access.public
    : f === "replicated" ? !!o.replication : f === "unreplicated" ? !o.replication : f === "encrypted" ? o.encrypted : f === "lifecycle" ? (o.lifecycle || []).length > 0 : true);
  const cfg = VIEWS[seg.t];
  const key = seg.t + "|" + (parent ? parent.id : "") + "|" + rev;
  const {data, loading, error, reload} = useResource(key, () => cfg.load(parent || {}), 6000);
  const acc = useAccess();
  // §3: what the caller cannot read is absent, not greyed
  const items = useMemo(() => (data || []).filter(o => acc.canRead(o)), [data, acc.state.user, acc.state.bindings]);
  const keys = SORT_KEYS[cfg.kind] || ["name"];
  const activeSort = keys.includes(sort) ? sort : keys[0];
  const filtered = useMemo(() => {
    const s = q.trim().toLowerCase();
    return items.filter(o => (!filters.length || filters.includes(o.status)) && (!s || searchable(o).includes(s))
        && (!xf.sc || o.storageClass === xf.sc)
        && (!xf.ann || Object.keys(o.annotations || {}).includes(xf.ann))
        && tagMatch(o, xf.tag) && (!xf.region || o.region === xf.region) && featMatch(o, xf.feat))
      .sort(SORTS[activeSort].cmp);
  }, [data, q, activeSort, filters, xf.sc, xf.ann, xf.tag, xf.region, xf.feat]);
  const T = TILE[cfg.kind], tk = TKEY[cfg.kind];
  // "New …" is a create on the entity that owns this layer, evaluated against
  // the parent object (a pool is created in a cluster, a cluster on a k8s cluster)
  const parentObj = parent && parent.id ? (REG[parent.id] || {kind: parent.t, id: parent.id}) : null;
  const createKind = seg.t === "clusters" ? "k8sc" : seg.t === "deployconfigs" ? "deployconfig" : cfg.kind;
  const mayCreate = seg.t === "clusters" ? acc.canAnywhere("create", "k8scluster") : acc.canCreateIn(createKind, parentObj);
  const bad = items.filter(o => healthOf(o) >= 3).length;
  const scope = parent && parent.id && parent.t !== "cluster" ? {t: parent.t, label: segLabel(parent)} : null;
  const prepScope = parent && parent.t === "cluster" ? parent.id : null;
  const candidates = cfg.kind === "host" ? items.filter(o => o.status === "discovered") : [];
  const isPvc = cfg.kind === "pvc";
  const isBucket = cfg.kind === "bucket";
  const regions = isBucket ? [...new Set(items.map(o => o.region).filter(Boolean))].sort() : [];
  const tagKeys = isBucket ? [...new Set(items.flatMap(o => Object.keys(o.tags || {})))].sort() : [];
  const scNames = isPvc ? [...new Set(items.map(o => o.storageClass))].sort() : [];
  const annKeys = isPvc ? [...new Set(items.flatMap(o => Object.keys(o.annotations || {})))].sort() : [];
  const select = cfg.kind === "host" ? {
    has: id => sel.includes(id),
    toggle: id => setSel(s => s.includes(id) ? s.filter(x => x !== id) : s.concat(id))
  } : null;
  const prepare = async () => {
    try { await api.hostsPrepare(prepScope || clusterOf(parent), sel); window.__toast(`Inspection pod scheduled on ${sel.length} node(s)`); setSel([]); reload(); }
    catch (e) { window.__toast(e.message); }
  };

  if (error) return <div className="scroll"><ErrorState error={error} onRetry={reload} kind={parent ? parent.t : cfg.kind} onUp={up} upLabel={upLabel} /></div>;
  return (
    <>
      <Toolbar {...{items, q, setQ, filters, setFilters, density, setDensity}} kind={cfg.kind} scope={scope}
        sort={activeSort} setSort={setSort} count={filtered.length} onRefresh={reload}
        extra={!mayCreate ? null : seg.t === "clusters" ? <button className="btn primary" onClick={() => window.__ui.dialog(deployFromDialog(nav), {kind: "cluster", id: "new"})}><Icon n="plus" s={12} />Deploy cluster</button>
          : seg.t === "deployconfigs" && parent && parent.t === "k8sc" ? <button className="btn primary" onClick={() => nav.deployWizard(parent.id)}><Icon n="plus" s={12} />Deploy a cluster</button>
          : seg.t === "pools" && parent && parent.t === "cluster" ? <button className="btn primary" onClick={() => window.__ui.dialog(newPoolDialog(REG[parent.id] || {id: parent.id, name: "this cluster"}), {kind: "pool", id: "new"})}><Icon n="plus" s={12} />New pool</button>
          : seg.t === "plans" ? <button className="btn primary" onClick={() => api.sites().then(ss => window.__ui.dialog(newPlanDialog(ss), {kind: "plan", id: "new"}))}><Icon n="plus" s={12} />New plan</button>
          : seg.t === "pairs" ? <button className="btn primary" onClick={() => window.__ui.dialog(newPairDialog(), {kind: "pair", id: "new"})}><Icon n="plus" s={12} />New pair</button>
          : seg.t === "rpolicies" ? <button className="btn primary" onClick={() => window.__ui.dialog(newReplPolicyDialog(parent && parent.t === "pair" ? REG[parent.id] : null), {kind: "rpolicy", id: "new"})}><Icon n="plus" s={12} />New policy</button>
          : seg.t === "__pairs_old" ? <button className="btn primary" onClick={() => window.__ui.dialog(newPairDialog(), {kind: "cluster pair", id: "new"})}><Icon n="plus" s={12} />Pair clusters</button>
          : seg.t === "mpaths" ? <button className="btn primary" onClick={() => window.__ui.dialog(newMPathDialog(), {kind: "migration path", id: "new"})}><Icon n="plus" s={12} />New migration path</button>
          : seg.t === "appgroups" && parent ? <button className="btn primary" onClick={() => window.__ui.dialog(newAppGroupDialog(REG[parent.id] || {id: parent.id}), {kind: "application group", id: "new"})}><Icon n="plus" s={12} />Add application group</button>
          : seg.t === "rpolicies" ? <button className="btn primary" onClick={() => window.__ui.dialog(newRPolicyDialog(), {kind: "replication policy", id: "new"})}><Icon n="plus" s={12} />New policy</button>
          : seg.t === "policies" && parent ? <button className="btn primary" onClick={() => window.__ui.dialog(newBackupPolicyDialog({id: parent.id}), {kind: "backup policy", id: "new"})}><Icon n="plus" s={12} />New policy</button>
          : seg.t === "protectedapps" && (!parent || !parent.id) ? <button className="btn primary" onClick={() => window.__ui.dialog(protectAppDialog(), {kind: "protected application", id: "new"})}><Icon n="shield" s={12} />Protect application</button>
          : isPvc ? <>
              <select className="sel" value={xf.sc} onChange={e => setXf(x => Object.assign({}, x, {sc: e.target.value}))} title="Filter by storage class">
                <option value="">All storage classes</option>
                {scNames.map(n => <option key={n} value={n}>{n}</option>)}
              </select>
              <select className="sel" value={xf.ann} onChange={e => setXf(x => Object.assign({}, x, {ann: e.target.value}))} title="Filter by annotation">
                <option value="">Any annotation</option>
                {annKeys.map(n => <option key={n} value={n}>{n}</option>)}
              </select>
              {(xf.sc || xf.ann) && <button className="chip" onClick={() => setXf(XF0)}><Icon n="x" s={10} />clear</button>}
            </>
          : isBucket ? <>
              <input className="sel" list="bucket-tag-keys" style={{width: 170}} placeholder="tag key or key=value" value={xf.tag}
                onChange={e => setXf(x => Object.assign({}, x, {tag: e.target.value}))} title="Filter by S3 bucket tag" />
              <datalist id="bucket-tag-keys">{tagKeys.map(k => <option key={k} value={k} />)}</datalist>
              {regions.length > 1 && <select className="sel" value={xf.region} onChange={e => setXf(x => Object.assign({}, x, {region: e.target.value}))} title="Filter by region">
                <option value="">All regions</option>{regions.map(r => <option key={r} value={r}>{r}</option>)}</select>}
              <select className="sel" value={xf.feat} onChange={e => setXf(x => Object.assign({}, x, {feat: e.target.value}))} title="Filter by bucket configuration">
                <option value="">Any configuration</option>
                <option value="versioned">Versioned</option><option value="locked">Object lock</option>
                <option value="encrypted">Encrypted</option><option value="public">Public</option>
                <option value="replicated">Replicated</option><option value="unreplicated">Not replicated</option>
                <option value="lifecycle">Has lifecycle rules</option>
              </select>
              {(xf.tag || xf.region || xf.feat) && <button className="chip" onClick={() => setXf(XF0)}><Icon n="x" s={10} />clear</button>}
              {parent && <button className="btn primary" onClick={() => window.__ui.dialog(newBucketDialog(REG[parent.id] || {id: parent.id, name: "cluster", objectStorage: {}}), {kind: "bucket", id: "new"})}><Icon n="plus" s={12} />New bucket</button>}
            </>
          : seg.t === "migrations" && parent && parent.t === "cluster" ? <button className="btn primary" onClick={() => window.__ui.dialog(newMigrationDialog(REG[parent.id] || {id: parent.id}), {kind: "migration", id: "new"})}><Icon n="move" s={12} />New migration</button>
          : seg.t === "cgroups" && parent ? <button className="btn primary" onClick={() => window.__ui.dialog(newCgroupDialog({id: parent.id}), {kind: "consistency group", id: "new"})}><Icon n="plus" s={12} />New group</button>
          : seg.t === "cgsnapshots" && parent ? <button className="btn primary" onClick={() => window.__ui.dialog(ACTIONS.cgroup(REG[parent.id] || {id: parent.id, name: "group", counts: {}})[0].dialog, {kind: "group snapshot", id: "new"})}><Icon n="camera" s={12} />Take snapshot</button>
          : seg.t === "hosts" && candidates.length ? <>
              <button className="chip" onClick={() => setSel(sel.length === candidates.length ? [] : candidates.map(c => c.id))}>
                {sel.length === candidates.length ? "Clear" : `Select all ${candidates.length}`}</button>
              <button className="btn primary" disabled={!sel.length} onClick={prepare}><Icon n="plus" s={12} />Prepare {sel.length || ""} node{sel.length === 1 ? "" : "s"}</button>
            </> : null} />
      <div className="scroll">
        {!loading && candidates.length > 0 && (
          <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}>
            <Icon n="host" s={15} /><span><b>{candidates.length} Kubernetes worker node(s) are not prepared.</b> Select the ones that should become storage hosts — an inspection pod collects their NUMA topology, devices and NICs before you configure them.</span></div>
        )}
        {!loading && bad > 0 && activeSort === "health" && !q && !filters.length && (
          <div className="banner"><Icon n="alert" s={15} /><span><b>{bad} of {items.length} need attention.</b> Sorted so non-healthy objects come first.</span></div>
        )}
        {loading ? <div className="grid">{Array.from({length: 8}).map((_, i) => <div className="skel" key={i}></div>)}</div>
          : items.length === 0 ? (
            <div className="empty"><Icon n={LAYER_META[seg.t].icon} s={24} /><b>No {kindPlural(cfg.kind)} yet</b>
              <span>The API returned an empty collection for this scope.</span>
              {seg.t === "deployconfigs" && parent && parent.t === "k8sc" && <button className="btn primary" style={{marginTop: 8}} onClick={() => nav.deployWizard(parent.id)}><Icon n="plus" s={12} />Deploy a cluster</button>}</div>
          ) : filtered.length === 0 ? (
            <div className="empty"><Icon n="search" s={22} /><b>No {kindPlural(cfg.kind)} match</b>
              <span>Adjust the search or clear the status filters.</span>
              <button className="chip" style={{marginTop: 6}} onClick={() => {setQ(""); setFilters([]);}}>Reset filters</button></div>
          ) : <div className="grid">{filtered.map(o => <T key={o.id} {...{[tk]: o, nav, select}} />)}</div>}
      </div>
    </>
  );
}

function DetailView({seg, nav, rev, up, upLabel}) {
  const {data, loading, error, reload} = useResource(seg.t + "|" + seg.id + "|" + rev, () => GETTER[seg.t](seg.id), 6000);
  if (error) return <div className="scroll"><ErrorState error={error} onRetry={reload} kind={seg.t} onUp={up} upLabel={upLabel} /></div>;
  if (loading || !data) return <div className="scroll"><div className="dhead"><div className="skel" style={{height: 26, width: 220}}></div></div>
    <div className="stats">{Array.from({length: 6}).map((_, i) => <div className="skel" key={i} style={{height: 62}}></div>)}</div>
    <div className="dcols"><div className="skel" style={{height: 300}}></div><div className="skel" style={{height: 300}}></div></div></div>;
  return <div className="scroll" style={{paddingTop: 2}}><Detail obj={data} nav={nav} /></div>;
}

function MockPanel() {
  const [open, setOpen] = useState(false);
  const [, f] = useState(0);
  const M = window.SB_MOCK;
  const set = (k, v) => { M[k] = v; f(n => n + 1); };
  const Row = ({label, on, onClick}) => <button className="menu-item" onClick={onClick}><span style={{flex: 1}}>{label}</span><span className="badge" style={on ? {color: "var(--accent)", borderColor: "var(--accent-line)"} : null}>{on ? "on" : "off"}</span></button>;
  return (
    <div className="switcher">
      <button className="swbtn" onClick={() => setOpen(!open)} title="Mock API controls"><Dot c="var(--warn)" />mock api<Icon n="chevd" s={9} /></button>
      {open && <>
        <div style={{position: "fixed", inset: 0, zIndex: 50}} onClick={() => setOpen(false)}></div>
        <div className="menu" style={{right: 0, left: "auto"}}>
          <div className="menu-lbl">Fixture backend · {M.requests} requests served</div>
          <Row label="Slow network (1.5–3 s)" on={M.latency[1] > 1000} onClick={() => set("latency", M.latency[1] > 1000 ? [140, 380] : [1500, 3000])} />
          <Row label="Kubernetes API offline" on={M.offline} onClick={() => set("offline", !M.offline)} />
          <Row label="Empty collections" on={M.forceEmpty} onClick={() => set("forceEmpty", !M.forceEmpty)} />
          <Row label="Random 503s (20%)" on={M.failRate > 0} onClick={() => set("failRate", M.failRate ? 0 : .2)} />
          <button className="menu-item" onClick={() => {set("failNext", true); setOpen(false);}}><span style={{flex: 1}}>Fail the next request</span><Icon n="alert" s={12} /></button>
          <div className="menu-lbl" style={{borderTop: "1px solid var(--line)", marginTop: 4, paddingTop: 8}}>Base URL</div>
          <div className="menu-item mono" style={{color: "var(--dim)", fontSize: 11}}>{API}</div>
        </div>
      </>}
    </div>
  );
}

class ViewBoundary extends React.Component {
  constructor(p) { super(p); this.state = {err: null}; }
  static getDerivedStateFromError(err) { return {err}; }
  componentDidUpdate(prev) { if (prev.routeKey !== this.props.routeKey && this.state.err) this.setState({err: null}); }
  render() {
    if (!this.state.err) return this.props.children;
    return (
      <div className="scroll"><div className="empty">
        <Icon n="alert" s={24} c="var(--bad)" />
        <b style={{color: "var(--text)"}}>This view failed to render</b>
        <span style={{maxWidth: 460}}>{String(this.state.err.message || this.state.err)}</span>
        <button className="chip" style={{marginTop: 8}} onClick={this.props.onReset}>Back to clusters</button>
      </div></div>
    );
  }
}

function App() {
  const [rawPath, setPath] = useLocal("sb.path", [{t: "clusters"}]);
  // a stored path may name a layer that no longer exists (renamed kinds) — fall back to the root
  const path = useMemo(() => Array.isArray(rawPath) && rawPath.length && rawPath.every(s => LAYER_META[s.t]) ? rawPath : [{t: "clusters"}], [rawPath]);
  const [theme, setTheme] = useLocal("sb.theme", "light");
  const [density, setDensity] = useLocal("sb.density", "340px");
  const [sort, setSort] = useLocal("sb.sort", "health");
  const [q, setQ] = useState("");
  const [filters, setFilters] = useState([]);
  const [menu, setMenu] = useState(false);
  const [toast, setToast] = useState(null);
  const [clusters, setClusters] = useState([]);
  const [alertCount, setAlertCount] = useState(0);
  const [rev, setRev] = useState(0);
  const [, force] = useState(0);

  useEffect(() => { document.documentElement.setAttribute("data-theme", theme); }, [theme]);
  useEffect(() => { document.documentElement.style.setProperty("--grid", density); }, [density]);
  useEffect(() => {
    window.__toast = m => { setToast(m); setTimeout(() => setToast(null), 2200); };
    window.__refresh = () => setRev(r => r + 1);
    window.__removed = obj => {
      const p = pathRef.current, last = p[p.length - 1];
      delete REG[obj.id];
      if (last && last.id === obj.id && p.length > 1) setPath(p.slice(0, -1));
      setRev(r => r + 1);
    };
  }, []);
  useEffect(() => { api.clusters().then(setClusters).catch(() => {}); }, [rev]);
  // zones are a small, cluster-independent collection; warm the registry once so
  // every regName(zoneId) call zone can label them without its own fetch
  useEffect(() => { api.zones().then(() => force(n => n + 1)).catch(() => {}); }, [rev]);
  // DR clusters are named all over the application-DR views but never appear in a
  // breadcrumb path, so warm them too
  useEffect(() => { api.drClusters().then(() => force(n => n + 1)).catch(() => {}); }, [rev]);
  useEffect(() => {
    const load = () => api.allAlerts().then(as => setAlertCount(as.filter(a => a.severity === "critical" && !a.silenced).length)).catch(() => {});
    load();
    const i = setInterval(load, 15000);
    return () => clearInterval(i);
  }, [rev]);

  const acc = useAccess();
  useEffect(() => {
    acc.load();
    // the fixture switcher changes identity in place; everything re-reads
    const h = () => { force(x => x + 1); setRev(r => r + 1); };
    window.addEventListener("sb:access", h); return () => window.removeEventListener("sb:access", h);
  }, []);
  const cur = path[path.length - 1];
  const viewKey = path.map(s => s.t + (s.id || "")).join("/");
  const pathRef = useRef(path); pathRef.current = path;
  useEffect(() => { setQ(""); setFilters([]); }, [viewKey]);
  useEffect(() => {
    const missing = path.filter(s => s.id && !REG[s.id]);
    if (missing.length) Promise.all(missing.map(s => resolve(s.t, s.id))).then(() => force(n => n + 1));
  }, [viewKey]);

  const go = useCallback(p => { setPath(p); setMenu(false); }, [setPath]);
  const nav = useMemo(() => ({
    detail: o => go(detailPath(o)),
    layer: (o, l) => go([...detailPath(o), {t: l}]),
    layerRef: (cid, l) => go([...pC(cid), {t: l}]),
    openCluster: cid => go(pC(cid)),
    openHost: (cid, hid) => go(pH(cid, hid)),
    openNode: (cid, nid) => go(pN(cid, nid)),
    openDevice: (cid, nid, did) => go(pD(cid, nid, did)),
    openPool: (cid, pid) => go(pP(cid, pid)),
    openVolume: (cid, pid, vid) => go(pV(cid, pid, vid)),
    openPair: id => go(pPair(id)),
    openRPolicy: id => go(pRPol(id)),
    openPolicy: (cid, id) => go([...pC(cid), {t: "policies"}, {t: "policy", id}]),
    openZone: id => go(pZone(id)),
    openK8s: id => go(pK(id)),
    openPlan: id => go(pPlan(id)),
    openSite: id => go(pSite(id)),
    // the plan and the application both name sites and methods as strings, the
    // way the CRs do, so the console resolves the name to its object
    openPairByName: name => api.pairs().then(ps => {
      const p = ps.find(x => x.name === name); if (p) go(pPair(p.id));
    }).catch(() => {}),
    openRPolicyByName: name => api.allRPolicies().then(ps => {
      const p = ps.find(x => x.name === name); if (p) go(pRPol(p.id));
    }).catch(() => {}),
    openSlotByName: name => api.slots().then(ss => {
      const s = ss.find(x => x.name === name); if (s) go(pSlot(s.id));
    }).catch(() => {}),
    openClusterByName: name => api.clusters().then(cs => {
      const c = cs.find(x => x.name === name); if (c) go(pC(c.id));
    }).catch(() => {}),
    openPvcByName: name => api.pvcs().then(ps => {
      const p = ps.find(x => x.name === name); if (p) go(detailPath(p));
    }).catch(() => {}),
    openPlanByName: name => api.plans().then(ps => {
      const p = ps.find(x => x.name === name); if (p) go(pPlan(p.id));
    }).catch(() => {}),
    openSiteByName: name => api.sites().then(ss => {
      const s = ss.find(x => x.name === name); if (s) go(pSite(s.id));
    }).catch(() => {}),
    openProtectedApp: id => go(pApp(id)),
    openBucket: id => (REG[id] ? Promise.resolve(REG[id]) : api.bucket(id)).then(x => go(pBucket(x.clusterId, x.id))).catch(() => {}),
    openPvc: id => (REG[id] ? Promise.resolve(REG[id]) : api.pvc(id)).then(x => go(pPvc(x.k8sClusterId, x.id))).catch(() => {}),
    openStorageClass: id => (REG[id] ? Promise.resolve(REG[id]) : api.storageClass(id)).then(x => go(pSc(x.k8sClusterId, x.id))).catch(() => {}),
    openVolumeById: id => (REG[id] ? Promise.resolve(REG[id]) : api.volume(id)).then(v => go(pV(v.clusterId, v.poolId, v.id))).catch(() => {}),
    k8s: () => go([{t: "k8s"}]),
    openCgroup: gid => (REG[gid] ? Promise.resolve(REG[gid]) : api.cgroup(gid)).then(g => go(pCg(g.clusterId, g.id))).catch(() => {}),
    drLayer: l => go([{t: "dr"}, {t: l}]),
    openMPath: id => go(pMp(id)),
    openAppGroup: (pid, id) => go(pAg(pid, id)),
    zones: () => go([{t: "dr"}, {t: "zones"}]),
    dr: () => go([{t: "dr"}]),
    cp: () => go([{t: "cp"}]),
    openSnapshot: (cid, sid) => (REG[sid] ? Promise.resolve(REG[sid]) : api.snapshot(sid)).then(s => go(detailPath(s))).catch(() => {}),
    hostNodes: h => go([...pH(h.clusterId, h.id), {t: "nodes"}]),
    discovery: kid => go([...pK(kid), {t: "discovery"}]),
    deployWizard: kid => go([...pK(kid), {t: "deploywizard"}]),
    deployConfig: (kid, id) => go(pDep(kid, id)),
    k8sDetail: kid => go(pK(kid)),
    root: () => go([{t: "clusters"}])
  }), [go]);

  useEffect(() => {
    const h = e => {
      if (/^(INPUT|SELECT|TEXTAREA)$/.test(e.target.tagName)) return;
      if (e.key === "Escape") {
        if (window.__ui && window.__ui.isOpen()) { window.__ui.close(); return; }
        if (path.length > 1) go(path.slice(0, -1));
        return;
      }
      if (e.key === "/") { e.preventDefault(); const el = document.querySelector(".search input"); el && el.focus(); }
    };
    window.addEventListener("keydown", h); return () => window.removeEventListener("keydown", h);
  }, [path, go]);

  const ctxCluster = useMemo(() => { const s = path.find(x => x.t === "cluster"); return s ? REG[s.id] : null; }, [viewKey, clusters.length]);
  const parentSeg = path[path.length - 2];
  const apiHint = cur.id ? DETAIL_API[cur.t] : VIEWS[cur.t] ? VIEWS[cur.t].api(parentSeg || {}) : "—";
  const s0 = path[0] && path[0].t;
  const section = s0 === "dr" ? "dr" : s0 === "k8s" ? "k8s" : s0 === "cp" ? "cp" : "clusters";
  const upOne = path.length > 1 ? () => go(path.slice(0, -1)) : null;
  const upLabel = path.length > 1 ? segLabel(path[path.length - 2]) : "";

  return (
    <div className="app">
      <div className="topbar">
        <a href="https://simplyblock.io" target="_blank" rel="noopener" style={{display: "flex", alignItems: "center"}} title="simplyblock">
          <img className="logo" src={(window.SB_CONFIG && window.SB_CONFIG.logoUrl) || "https://simplyblock.io/assets/images/Logo-white.svg"} alt="simplyblock"
            onError={e => { e.target.style.display = "none"; e.target.nextSibling.style.display = "block"; }} />
          <span className="logofb" style={{display: "none"}}>simplyblock</span>
        </a>
        <span className="prod">Control Center</span>
        <div className="sectionsw">
          <button className={section === "clusters" ? "on" : ""} onClick={() => nav.root()} title="Clusters"><Icon n="cluster" s={13} /><span className="swlabel">Clusters</span></button>
          {acc.canAnywhere("read", "k8scluster") && <button className={section === "k8s" ? "on" : ""} onClick={() => nav.k8s()} title="Kubernetes"><Icon n="k8s" s={13} /><span className="swlabel">Kubernetes</span></button>}
          {(acc.canAnywhere("read", "drpolicy") || acc.canAnywhere("read", "replicationpolicy") || acc.canAnywhere("read", "application")) && <button className={section === "dr" ? "on" : ""} onClick={() => nav.dr()} title="Disaster recovery"><Icon n="shield" s={13} /><span className="swlabel">Disaster recovery</span></button>}
          <button className={section === "cp" ? "on" : ""} onClick={() => nav.cp()} title="Control plane"><Icon n="host" s={13} /><span className="swlabel">Control plane</span></button>
        </div>
        {section === "clusters" && <div className="switcher">
          <button className="swbtn" onClick={() => setMenu(!menu)}>
            {ctxCluster ? <><Dot c={STATUS_META[ctxCluster.status].c} />{ctxCluster.name}</> : <><Icon n="cluster" s={13} />All clusters</>}
            <Icon n="chevd" s={9} />
          </button>
          {menu && <>
            <div style={{position: "fixed", inset: 0, zIndex: 50}} onClick={() => setMenu(false)}></div>
            <div className="menu">
              <div className="menu-lbl">Managed clusters · {clusters.length}</div>
              <button className="menu-item" onClick={() => nav.root()}><Icon n="cluster" s={13} c="var(--dim)" />All clusters</button>
              {clusters.map(c => (
                <button key={c.id} className={"menu-item" + (ctxCluster && ctxCluster.id === c.id ? " on" : "")} onClick={() => nav.openCluster(c.id)}>
                  <Dot c={STATUS_META[c.status].c} /><span style={{flex: 1}}>{c.name}</span>
                  <span className="badge">{c.siting === "edge" ? "edge" : "dc"}</span>
                </button>
              ))}
            </div>
          </>}
        </div>}
        <div className="spacer"></div>
        {window.SB_CONFIG.mock && <MockPanel />}
        <span className="env"><span className="pulse"></span>control plane healthy</span>
        <button className="tbtn bellwrap" title={alertCount ? `${alertCount} critical alert(s)` : "No critical alerts"}
          onClick={() => { const s = path.find(x => x.t === "cluster"); if (s) nav.openCluster(s.id); else if (clusters.length) nav.openCluster(clusters.find(c => c.status === "degraded") ? clusters.find(c => c.status === "degraded").id : clusters[0].id); }}>
          <Icon n="bell" s={14} />{alertCount > 0 && <span className="bellbadge">{alertCount > 99 ? "99+" : alertCount}</span>}
        </button>
        <button className="tbtn" title="Toggle theme" onClick={() => setTheme(theme === "light" ? "dark" : "light")}><Icon n={theme === "light" ? "moon" : "sun"} s={14} /></button>
        <IdentityMenu here={cur.id ? REG[cur.id] : null} />
      </div>

      <div className="crumbbar">
        <div className="crumbs">
          {path.map((s, i) => {
            const last = i === path.length - 1;
            return (
              <React.Fragment key={i}>
                {i > 0 && <span className="csep"><Icon n="chev" s={12} sw={1.6} /></span>}
                <button className={"crumb" + (last ? " cur" : "")} onClick={() => !last && go(path.slice(0, i + 1))}>
                  <span className="ic"><Icon n={LAYER_META[s.t].icon} s={13} /></span>{segLabel(s)}
                </button>
              </React.Fragment>
            );
          })}
        </div>
        <span className="apihint" title={apiHint}>{apiHint}</span>
      </div>

      {!acc.state.ready ? <div className="scroll"><div className="lmsg">Resolving your access…</div></div> :
      <ViewBoundary routeKey={viewKey} onReset={() => nav.root()}>
      {cur.id
        ? <DetailView key={viewKey} seg={cur} nav={nav} rev={rev} up={upOne} upLabel={upLabel} />
        : cur.t === "dr"
        ? <DrHome key={viewKey} nav={nav} />
        : cur.t === "cp"
        ? <ControlPlaneView key={viewKey} nav={nav} />
        : cur.t === "discovery"
        ? <DiscoveryView key={viewKey} kid={(path.find(x => x.t === "k8sc") || {}).id} nav={nav} />
        : cur.t === "deploywizard"
        ? <DeployWizard key={viewKey} kid={(path.find(x => x.t === "k8sc") || {}).id} nav={nav} />
        : <OverviewView key={viewKey} seg={cur} parent={parentSeg} nav={nav} rev={rev} up={upOne} upLabel={upLabel}
            prefs={{q, setQ, sort, setSort, filters, setFilters, density, setDensity}} />}
      </ViewBoundary>}
      <UiLayer routeKey={viewKey} />
      {toast && <div className="toast">{toast}</div>}
    </div>
  );
}

ReactDOM.createRoot(document.getElementById("root")).render(<App />);
