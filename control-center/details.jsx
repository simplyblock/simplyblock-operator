const Props = ({rows}) => (
  <dl className="props">{rows.filter(Boolean).map(([k, v], i) => (
    <React.Fragment key={i}><dt>{k}</dt><dd>{v === null || v === undefined || v === "" ? <span style={{color: "var(--dim2)"}}>—</span> : v}</dd></React.Fragment>
  ))}</dl>
);
const Stat = ({k, v, s, c}) => {
  const n = typeof v === "string" || typeof v === "number" ? String(v).length : 0;
  return (
    <div className="stat">
      <div className="k" title={k}>{k}</div>
      <div className={"v" + (n > 22 ? " xs" : n > 15 ? " sm" : n > 10 ? " md" : "")} style={{color: c}} title={n > 10 ? String(v) : null}>{v}</div>
      {s && <div className="s">{s}</div>}
    </div>
  );
};
const Ref = ({onClick, label}) => <button onClick={onClick} style={{color: "var(--accent)", fontFamily: "inherit"}}>{label}</button>;
const regName = (id, fb) => { const o = REG[id]; return o ? (o.hostname || o.name) : (fb || (id ? shortId(id) : "—")); };
const NavCard = ({icon, title, sub, count, onClick}) => (
  <button className="navcard" onClick={onClick}>
    <div><div className="t"><Icon n={icon} s={14} c="var(--accent)" />{title}</div><div className="sb">{sub}</div></div>
    <span className="c">{count}</span>
  </button>
);
const NicTable = ({nics, mgmt, data}) => (
  <div className="card"><div className="bd" style={{padding: 0}}>
    <table className="dt"><thead><tr><th>Interface</th><th>Address</th><th>MAC</th><th>Speed</th><th>Socket</th><th>State</th><th>Role</th></tr></thead>
      <tbody>{(nics || []).map(n => (
        <tr key={n.name}>
          <td className="mono" style={{fontWeight: 600}}>{n.name}</td>
          <td className="mono">{n.address}</td>
          <td className="mono" style={{color: "var(--dim)"}}>{n.mac}</td>
          <td className="mono">{n.speed} GbE</td>
          <td className="mono">{n.socket}</td>
          <td><TrafficLight status={n.state === "up" ? "online" : "offline"} /></td>
          <td>{mgmt === n.name ? <span className="badge k8s">management</span>
            : (data || []).includes(n.name) ? <span className="badge">data</span>
            : <span style={{color: "var(--dim2)"}}>—</span>}</td>
        </tr>
      ))}</tbody></table>
  </div></div>
);

function Chart({title, data, color, fmt, unit}) {
  if (!data || data.length < 2) return null;
  const w = 300, h = 62;
  const max = Math.max(...data, 1), min = Math.min(...data);
  const rng = max - min || max || 1;
  const line = data.map((v, i) => `${(i / (data.length - 1)) * w},${h - ((v - min) / rng) * (h - 8) - 4}`).join(" ");
  const gid = "g" + title.replace(/\W/g, "");
  return (
    <div style={{marginBottom: 14}}>
      <div style={{display: "flex", justifyContent: "space-between", alignItems: "baseline", marginBottom: 5}}>
        <span style={{fontSize: 11, color: "var(--dim)", letterSpacing: ".05em", textTransform: "uppercase"}}>{title}</span>
        <span className="mono" style={{fontSize: 12}}>{fmt(data[data.length - 1])}{unit && <span style={{color: "var(--dim2)"}}> {unit}</span>}</span>
      </div>
      <svg width="100%" height={h} viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" style={{display: "block", overflow: "visible"}}>
        <defs><linearGradient id={gid} x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stopColor={color} stopOpacity=".22" /><stop offset="100%" stopColor={color} stopOpacity="0" /></linearGradient></defs>
        <polygon points={`0,${h} ${line} ${w},${h}`} fill={`url(#${gid})`} />
        <polyline points={line} fill="none" stroke={color} strokeWidth="1.5" vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
      </svg>
    </div>
  );
}

const IOCards = ({o}) => (
  <div className="card"><h3>Live I/O · /iostats, last 28 samples</h3><div className="bd">
    <Chart title="IOPS (read + write)" data={o.hist.iops} color="var(--accent)" fmt={fmtNum} />
    <Chart title="Throughput" data={o.hist.bw} color="var(--ok)" fmt={fmtBW} unit="GB/s" />
    <div className="stats" style={{marginTop: 4}}>
      <Stat k="IOPS read" v={fmtNum(o.iops.r)} /><Stat k="IOPS write" v={fmtNum(o.iops.w)} />
      <Stat k="Read" v={fmtBW(o.bw.r)} s="GB/s" /><Stat k="Write" v={fmtBW(o.bw.w)} s="GB/s" />
    </div>
  </div></div>
);

const DetailHead = ({obj, title, sub, badge}) => (
  <div className="dhead">
    <div style={{minWidth: 0, flex: 1}}>
      <div style={{display: "flex", alignItems: "center", gap: 11, flexWrap: "wrap"}}><h1>{title}</h1><TrafficLight status={obj.status} />{badge}</div>
      <div style={{display: "flex", alignItems: "center", gap: 14, marginTop: 5, flexWrap: "wrap"}}><Uuid value={obj.id} short={false} />{sub}</div>
    </div>
    <ActionBtn obj={obj} big />
  </div>
);

const QosProps = ({qos, fallback}) => qos ? <Props rows={[
  ["Max R/W IOPS", qos.rw_ios_per_sec ? fmtNum(qos.rw_ios_per_sec) : "unlimited"],
  ["Max R/W throughput", qos.rw_mbytes_per_sec ? qos.rw_mbytes_per_sec + " MB/s" : "unlimited"],
  ["Max read throughput", qos.r_mbytes_per_sec ? qos.r_mbytes_per_sec + " MB/s" : "unlimited"],
  ["Max write throughput", qos.w_mbytes_per_sec ? qos.w_mbytes_per_sec + " MB/s" : "unlimited"]
]} /> : <div className="nolim">{fallback}</div>;

// The connection fields differ per provider — a Vault transit mount means
// nothing to AWS KMS, so only emit the rows that provider actually has.
const KMS_LABEL = {hashicorp_vault: "HashiCorp Vault", aws_kms: "AWS KMS",
  azure_key_vault: "Azure Key Vault", gcp_kms: "Google Cloud KMS", kmip: "KMIP appliance"};
const KMS_ADDR = {hashicorp_vault: "Vault address", aws_kms: "Endpoint",
  azure_key_vault: "Vault URI", gcp_kms: "Key ring", kmip: "KMIP endpoint"};
const KMS_ROLE = {hashicorp_vault: "Role", aws_kms: "Role ARN",
  azure_key_vault: "Client / tenant id", gcp_kms: "Role", kmip: "Client certificate secret"};
function KMS_ROWS(k) {
  const p = k.provider;
  return [
    ["Provider", KMS_LABEL[p] || p],
    ["Status", <TrafficLight status={k.status === "connected" ? "online" : k.status === "sealed" ? "suspended" : "unreachable"} />],
    [KMS_ADDR[p] || "Address", k.address],
    p === "aws_kms" ? ["Region", k.region] : null,
    p === "hashicorp_vault" ? ["Namespace", k.namespace] : null,
    ["Auth method", k.auth_method],
    k.auth_role ? [KMS_ROLE[p] || "Role", k.auth_role] : null,
    p === "hashicorp_vault" ? ["Transit mount", k.mount_path + "/"] : null,
    ["Key", k.key_name], ["Key type", k.key_type],
    ["Rotation", k.rotation_days ? "every " + k.rotation_days + " days" : "manual"],
    ["Verify TLS", k.verify_tls ? "yes" : "no"],
    ["Keys in use", k.keys_in_use + " encrypted volume(s)"],
    ["Scope", "every encrypted volume and bucket in this cluster"],
    ["Last check", fmtDate(k.last_check_at) + " · " + fmtAgo(k.last_check_at)]
  ].filter(Boolean);
}

function ClusterDetail({o: c, nav}) {
  const [tab, setTab] = useState("overview");
  return (
    <div>
      <DetailHead obj={c} title={c.name}
        badge={<><span className="badge">{c.siting === "edge" ? "edge" : "data center"}</span>
          <span className="badge">{c.mode === "nvme" ? "NVMe" : "block device"}</span></>}
        sub={c.caps.rebalancing
          ? <span style={{fontSize: 11.5, color: "var(--dim)", display: "flex", alignItems: "center", gap: 6}}>rebalancing<Dot c={c.rebalancing ? "var(--warn)" : "var(--ok)"} />{c.rebalancing ? "in progress" : "idle"}</span>
          : <span style={{fontSize: 11.5, color: "var(--dim2)"}}>edge deployment — no rebalancing, no task engine</span>} />
      <Tabs active={tab} onChange={setTab} items={[
        {k: "overview", label: "Overview", icon: "cluster"},
        {k: "alerts", label: "Alerts", icon: "bell"},
        {k: "ops", label: "Operations", icon: "refresh"},
        {k: "file", label: "File storage", icon: "folder"},
        c.caps.tasks && {k: "tasks", label: "Tasks", icon: "gauge"},
        {k: "logs", label: "Cluster log", icon: "filter"},
        {k: "splane", label: "Storage plane logs", icon: "node"},
      ].filter(Boolean)} />
      {tab === "alerts" && <AlertsPanel cluster={c} nav={nav} />}
      {tab === "ops" && <OperationsPanel cluster={c} nav={nav} />}
      {tab === "file" && <FileStoragePanel cluster={c} nav={nav} />}
      {tab === "tasks" && c.caps.tasks && <TasksPanel cluster={c} />}
      {tab === "logs" && <ClusterLogPanel cluster={c} />}
      {tab === "splane" && <StoragePlaneLogPanel cluster={c} />}
      {tab === "overview" && <>
      {c.status === "degraded" && <div className="banner"><Icon n="alert" s={15} /><span><b>Cluster degraded.</b> {c.counts.nodes - c.counts.nodesOnline} of {c.counts.nodes} storage nodes are not online
        {c.faultBudget.kind === "failure_domain" ? ` in ${c.faultBudget.lost} failure domain` : ""} — inside the fault budget of {c.faultBudget.kind === "failure_domain" ? "one failure domain" : `${c.faultBudget.tolerated} node${c.faultBudget.tolerated === 1 ? "" : "s"}`}, capacity is served with reduced redundancy. Losing {c.faultBudget.kind === "failure_domain" ? "a second domain" : "one more node"} suspends the cluster.</span></div>}
      {c.status === "suspended" && <div className="banner"><Icon n="alert" s={15} /><span><b>Cluster suspended.</b> {c.counts.nodesOnline === 0
        ? "All storage nodes are stopped. Restart the cluster to resume serving volumes."
        : `${c.counts.nodes - c.counts.nodesOnline} of ${c.counts.nodes} storage nodes are not online${c.faultBudget.kind === "failure_domain" ? ` across ${c.faultBudget.lost} failure domains` : ""} — more than the fault budget of ${c.faultBudget.kind === "failure_domain" ? "one failure domain" : c.faultBudget.tolerated + " node" + (c.faultBudget.tolerated === 1 ? "" : "s")}. I/O is halted until nodes return; the cluster resumes as degraded once the loss is back inside the budget.`}</span></div>}
      <div className="stats">
        <Stat k="Capacity used" v={pct(c.capacity.used, c.capacity.total).toFixed(0) + "%"} s={`${fmtBytes(c.capacity.used)} of ${fmtBytes(c.capacity.total)}`} />
        <Stat k="Total IOPS" v={fmtNum(c.iops.r + c.iops.w)} s={`${fmtNum(c.iops.r)} r · ${fmtNum(c.iops.w)} w`} />
        <Stat k="Throughput" v={fmtBW(c.bw.r + c.bw.w)} s="GB/s combined" />
        <Stat k="Nodes online" v={`${c.counts.nodesOnline}/${c.counts.nodes}`} />
        <Stat k="Hosts" v={`${c.counts.hostsAvailable}/${c.counts.hosts}`} s="available" />
        <Stat k="Devices" v={c.counts.devices} s={`${c.counts.devicesOnline} online`} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="host" title="Hosts" sub="prepared machines" count={c.counts.hosts} onClick={() => nav.layer(c, "hosts")} />
        <NavCard icon="node" title="Storage nodes" sub="devices &amp; I/O" count={c.counts.nodes} onClick={() => nav.layer(c, "nodes")} />
        <NavCard icon="pool" title="Storage pools" sub="QoS &amp; tenancy" count={c.counts.pools} onClick={() => nav.layer(c, "pools")} />
        <NavCard icon="volume" title="Logical volumes" sub="across all pools" count={c.counts.volumes} onClick={() => nav.layer(c, "volumes")} />
        <NavCard icon="camera" title="Snapshots" sub="all pools" count={c.counts.snapshots} onClick={() => nav.layer(c, "snapshots")} />
        <NavCard icon="cloud" title="Backups" sub="object storage" count={c.counts.backups} onClick={() => nav.layer(c, "backups")} />
        <NavCard icon="clock" title="Backup policies" sub="schedules &amp; retention" count={c.counts.policies} onClick={() => nav.layer(c, "policies")} />
        <NavCard icon="shield" title="Replication policies" sub={c.caps.async_replication ? "DR & synchronous" : "Kubernetes only"}
          count={c.caps.async_replication ? c.counts.rpolicies : "n/a"} onClick={() => nav.layer(c, "rpolicies")} />
        <NavCard icon="link" title="Consistency groups" sub="grouped volumes &amp; group snapshots" count={c.counts.cgroups} onClick={() => nav.layer(c, "cgroups")} />
        <NavCard icon="move" title="Migrations" sub="move storage to other nodes or clusters" count={c.counts.migrations} onClick={() => nav.layer(c, "migrations")} />
        {c.objectStorage.enabled && <NavCard icon="cloud" title="Buckets" sub="S3 on cluster capacity" count={c.counts.buckets} onClick={() => nav.layer(c, "buckets")} />}
        <NavCard icon="zone" title="Zones" sub={c.stretched ? "stretched cluster" : "single zone"} count={c.counts.zones} onClick={() => nav.layer(c, "zones")} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Cluster configuration</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Label", c.name], ["Siting", c.siting === "edge" ? "edge" : "data center"],
            ["Device class", c.mode === "nvme" ? "NVMe" : "block device"],
            ["Status", <TrafficLight status={c.status} />],
            ["Rebalancing", c.caps.rebalancing ? (c.rebalancing ? "yes" : "no") : "not available on edge clusters"],
            ["Task engine", c.caps.tasks ? "yes" : "not available on edge clusters"],
            ["Stripe", `${c.distrNdcs} data + ${c.distrNpcs} parity chunks`], ["Software version", `${c.version} · set by the operator release`],
            ["Regions", (c.regions || []).join(", ") || null],
            ["Zones", (c.zoneIds || []).length
              ? <span style={{display: "flex", gap: 8, justifyContent: "flex-end", flexWrap: "wrap"}}>{c.zoneIds.map(id => <Ref key={id} onClick={() => nav.openZone(id)} label={regName(id, "zone")} />)}</span>
              : null],
            ["Stretched", c.stretched ? `yes — ${(c.zoneIds || []).length} zones` : "no (single zone)"],
            ["DR target eligible", c.drEligible ? "yes" : "no"],
            [c.mgmtKind === "Kubernetes API" ? "Kubernetes API" : "Management endpoint", c.mgmt],
            ["Created", fmtDate(c.createdAt)]
          ]} /></div></div>
        <IOCards o={c} />
      </div>
      <div className="dcols" style={{marginTop: 12}}>
        <div>
          <div className="card"><h3>Feature flags · fixed at creation</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
            <Props rows={[
              ["Multipathing", c.multipathing
                ? `enabled · ${c.multipathNodes}/${c.counts.nodes} nodes on two paths`
                : "disabled · single data NIC per node"],
              ["Node affinity", c.nodeAffinity === "none" ? "none — free placement"
                : c.nodeAffinity === "strict" ? "strict — volumes can be pinned" : "soft — prefer to keep in place"],
              ["Pod affinity", c.podAffinity ? "enabled — front storage follows the workload" : "disabled"],
              ["Failure domains", c.fd.enabled ? `enabled · per ${c.fd.scope}` : "disabled"],
              ["Synchronous replication", c.syncReplication ? "enabled" : "disabled"],
                ["Backups", c.backupEnabled ? "enabled" : "disabled"],
              ["File storage (RWX)", c.fileStorage.enabled
                ? "pNFS · " + c.counts.rwxPvcs + " claim(s) · MDS " + c.fileStorage.mds_state
                : "disabled"],
              ["Object storage (S3)", c.objectStorage.enabled
                ? c.counts.buckets + " bucket(s) · metadata in FoundationDB"
                : "disabled"],
              ["Zones", (c.zoneIds || []).length]
            ]} />
            <div className="fnote" style={{marginTop: 10, marginBottom: 2}}><Icon n="alert" s={12} />
              Zones, failure domains and synchronous replication cannot be changed after the cluster is created.</div>
          </div></div>
          {c.fd.enabled && <div className="card" style={{marginTop: 12}}>
            <h3>Failure domains · per {c.fd.scope}</h3>
            <div className="bd" style={{padding: 0}}>
              <table className="dt"><thead><tr><th>{c.fd.scope}</th><th style={{textAlign: "right"}}>Nodes</th><th>Balance</th></tr></thead>
                <tbody>{c.fd.domains.map(d => (
                  <tr key={d.name}>
                    <td className="mono" style={{fontWeight: 600, color: d.name === "unassigned" ? "var(--bad)" : undefined}}>{d.name}</td>
                    <td className="mono" style={{textAlign: "right"}}>{d.nodes}</td>
                    <td><div className="bar" style={{maxWidth: 140}}><i style={{width: (d.nodes / Math.max(1, c.fd.max)) * 100 + "%",
                      "--bc": d.name === "unassigned" ? "var(--bad)" : d.nodes === c.fd.max ? "var(--accent)" : "var(--ok)"}}></i></div></td>
                  </tr>
                ))}</tbody></table>
              <div className="fnote" style={{margin: 12}}>
                <Icon n={c.fd.balanced ? "check" : "alert"} s={12} />
                {c.fd.unassigned
                  ? `${c.fd.unassigned} node(s) sit in no failure domain — their host carries no ${c.fd.scope} taint.`
                  : (c.fd.thin || []).length
                  ? `${(c.fd.thin || []).join(", ")} carries a single node. Each ${c.fd.scope} must hold at least two storage nodes, so a domain can lose one without losing its share of the data.`
                  : c.fd.domains.length < 2
                  ? `Only one ${c.fd.scope} is in use. Failure domains need at least two, each with at least two nodes.`
                  : c.fd.balanced
                  ? `Balanced: ${c.fd.min}–${c.fd.max} nodes per ${c.fd.scope}, at least two each. Nodes are added and removed in pairs, and the spread must stay within one.`
                  : `Unbalanced: ${c.fd.min}–${c.fd.max} nodes per ${c.fd.scope}. Node counts may differ by at most one.`}
              </div>
            </div>
          </div>}
          {c.caps.rebalancing && <div className="card" style={{marginTop: 12}}>
            <h3>Volume rebalancing</h3>
            <div className="bd">
              <Props rows={[
                ["Automatic rebalancing", c.autoRebalance.enabled ? "on" : "off — rebalance manually"],
                ["Volumes moved, last hour", c.autoRebalance.moved1h],
                ["Volumes moved, last 24 hours", c.autoRebalance.moved24h]
              ]} />
              <div className="fnote" style={{marginTop: 8, marginBottom: 0}}><Icon n="move" s={12} />
                A volume moves by instant migration: the primary role is handed to another node without copying data. Every move files an <span className="mono">lvol_migration</span> task — the Tasks tab is where an individual move is followed.</div>
            </div>
          </div>}
        </div>
        <div>
          <div className="card"><h3>Encryption · KMS</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
            {c.kms ? <Props rows={KMS_ROWS(c.kms)} />
              : <div className="nolim">No KMS configured. Encrypted volumes and buckets cannot be created in this cluster until an external key manager is set up.</div>}
          </div></div>
          {c.objectStorage.enabled && <div className="card" style={{marginTop: 12}}><h3>Object storage service · S3</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
            <Props rows={[
              ["Endpoint", c.objectStorage.endpoint], ["Region", c.objectStorage.region],
              ["Addressing", c.objectStorage.addressing],
              ["Metadata backend", "FoundationDB — the control plane state database"],
              ["Buckets", c.counts.buckets + " of " + c.objectStorage.max_buckets],
              ["Default versioning", c.objectStorage.versioning_default ? "on" : "off"]
            ]} />
            <div className="fnote" style={{marginTop: 10, marginBottom: 2}}><Icon n="alert" s={12} />
              This is the S3 service the cluster serves to its tenants. The S3 target the cluster writes its own backups to is configured separately below.</div>
          </div></div>}
          {c.backupEnabled && <div className="card" style={{marginTop: 12}}><h3>Backup target · S3</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
            {c.s3 ? <Props rows={[
              ["Endpoint", c.s3.endpoint], ["Region", c.s3.region], ["Bucket", c.s3.bucket],
              ["Path prefix", c.s3.path_prefix], ["Access key id", c.s3.access_key_id],
              ["Secret", c.s3.secret_access_key], ["Addressing", c.s3.addressing],
              ["Verify TLS", c.s3.verify_tls ? "yes" : "no"]
            ]} /> : <div className="nolim">Backups are enabled but no S3 endpoint is configured — backups will fail until one is set.</div>}
          </div></div>}
        </div>
      </div>
      </>}
    </div>
  );
}

function HostDetail({o: h, nav}) {
  const candidate = h.status === "discovered" || h.status === "inspecting" || h.status === "inspected";
  const sockets = h.socketsUsed && h.socketsUsed.length ? h.socketsUsed : Array.from({length: h.sockets || 0}, (_, s) => s);
  if (candidate) return (
    <div>
      <DetailHead obj={h} title={h.hostname}
        badge={<><span className="badge k8s">kubernetes worker</span><span className="badge">{h.kubelet}</span></>}
        sub={<span className="mono" style={{fontSize: 11.5, color: "var(--dim)"}}>{h.mgmtIp}</span>} />
      {h.status === "discovered" && <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}>
        <Icon n="host" s={15} /><span><b>Not prepared.</b> This worker node is visible to the operator but simplyblock knows nothing about its devices yet. Deploy the inspection pod to collect the NUMA topology, devices and NICs.</span></div>}
      {h.status === "inspecting" && <div className="banner" style={{color: "var(--info)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}>
        <Icon n="clock" s={15} /><span><b>Inspection running.</b> {h.inspection ? h.inspection.pod : "The inspection pod"} is collecting the device and NUMA inventory.</span></div>}
      {h.status === "inspected" && <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}>
        <Icon n="check" s={15} /><span><b>Inventory collected.</b> Choose the NUMA sockets, the memory per storage-plane pod, the devices and the NICs to finish preparing this host.</span></div>}
      <div className="stats">
        <Stat k="vCPU / cores" v={h.vcpu} />
        <Stat k="System RAM" v={fmtBytes(h.memory, 0)} />
        <Stat k="NUMA sockets" v={h.sockets || "unknown"} />
        <Stat k="Devices found" v={h.sockets ? h.devices.length : "—"} />
        <Stat k="NICs found" v={h.nics.length || "—"} />
      </div>
      {h.status === "inspected" && <>
        <div className="sech"><h2>Discovered devices</h2><span className="ln"></span></div>
        <div className="card"><div className="bd" style={{padding: 0}}>
          <table className="dt"><thead><tr><th>Socket</th><th>Kind</th><th>PCIe</th><th>Block device</th><th>Model</th><th style={{textAlign: "right"}}>Size</th></tr></thead>
            <tbody>{h.devices.map(d => (
              <tr key={d.id}><td className="mono">{d.socket}</td><td><span className="badge">{d.kind}</span></td>
                <td className="mono">{d.pcie || "—"}</td><td className="mono">{d.blockdev || "—"}</td>
                <td style={{color: "var(--dim)"}}>{d.model}</td>
                <td className="mono" style={{textAlign: "right"}}>{fmtBytes(d.size)}</td></tr>
            ))}</tbody></table>
        </div></div>
        <div className="sech"><h2>Discovered NICs</h2><span className="ln"></span></div>
        <NicTable nics={h.nics} />
      </>}
      <div className="dcols">
        <div className="card"><h3>Kubernetes node</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Node name", h.hostname], ["Kubelet", h.kubelet], ["Roles", h.roles.join(", ")],
            ["Zone", h.zoneId ? <Ref onClick={() => nav.openZone(h.zoneId)} label={h.zone || regName(h.zoneId, "zone")} /> : null],
            ["Region", h.region], ["Rack", h.rack], ["Cabinet", h.cabinet],
            ["Status", <TrafficLight status={h.status} />], ["Address", h.mgmtIp],
            ["Cluster", <Ref onClick={() => nav.openCluster(h.clusterId)} label={regName(h.clusterId)} />]
          ]} /></div></div>
        <div className="card"><h3>Node labels</h3><div className="bd">
          <div className="labels">{Object.entries(h.k8sLabels).map(([k, v]) => <span className="lab" key={k}><i>{k}</i>{v}</span>)}</div>
        </div></div>
      </div>
    </div>
  );
  return (
    <div>
      <DetailHead obj={h} title={h.hostname}
        badge={<>{h.controlPlane && <span className="badge k8s">control plane services</span>}
          <span className="badge">{h.counts.nodes ? `${h.counts.nodes} storage node(s)` : "no storage node"}</span></>}
        sub={<span className="mono" style={{fontSize: 11.5, color: "var(--dim)"}}>{h.mgmtIp}</span>} />
      {h.status === "unreachable" && <div className="banner"><Icon n="alert" s={15} /><span><b>Host unreachable.</b> The agent has stopped reporting. Storage nodes on this host cannot be restarted until it returns.</span></div>}
      {h.counts.nodes === 0 && h.status === "available" && <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}>
        <Icon n="plus" s={15} /><span><b>Prepared and labelled.</b> {h.counts.free} unassigned device(s) — this host can take a new storage node or receive a migrated one.</span></div>}
      <div className="stats">
        <Stat k="NUMA sockets" v={h.socketsUsed && h.socketsUsed.length ? `${h.socketsUsed.length} of ${h.sockets}` : h.sockets} s={h.socketsUsed && h.socketsUsed.length ? `socket ${h.socketsUsed.join(", ")} in use` : "all in use"} />
        <Stat k="vCPU / cores" v={h.vcpu} />
        <Stat k="System RAM" v={fmtBytes(h.memory, 0)} />
        <Stat k="Hugepages" v={fmtBytes(h.hugepages.allocated, 0)} s={`of ${fmtBytes(h.hugepages.reserved, 0)} reserved`} />
        <Stat k="Devices" v={`${h.counts.assigned}/${h.counts.devices}`} s="assigned" />
        <Stat k="Raw capacity" v={fmtBytes(h.capacity.total)} s={`${fmtBytes(h.capacity.used)} claimed`} />
      </div>
      <div className="sech"><h2>Devices by NUMA socket</h2><span className="ln"></span><span className="count">{h.counts.free} unassigned</span></div>      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Socket</th><th>Kind</th><th>PCIe</th><th>Block device</th><th>Model</th><th style={{textAlign: "right"}}>Size</th><th>Assignment</th></tr></thead>
          <tbody>
            {sockets.map(s => h.devices.filter(d => d.socket === s).map(d => (
              <tr key={d.id}>
                <td className="mono">{d.socket}</td>
                <td><span className="badge">{d.kind}</span></td>
                <td className="mono">{d.pcie || "—"}</td>
                <td className="mono">{d.blockdev || "—"}</td>
                <td style={{color: "var(--dim)"}}>{d.model}</td>
                <td className="mono" style={{textAlign: "right"}}>{fmtBytes(d.size)}</td>
                <td>{d.assignedNodeId
                  ? <Ref onClick={() => nav.openNode(h.clusterId, d.assignedNodeId)} label={regName(d.assignedNodeId, "storage node")} />
                  : d.reserved ? <span style={{color: "var(--warn)"}}>reserved · awaiting node restart</span>
                  : <span style={{color: "var(--dim2)"}}>unassigned</span>}</td>
              </tr>
            )))}
            {h.devices.filter(d => d.kind === "block" && !d.assignedNodeId).length === 0 && null}
          </tbody></table>
      </div></div>
      <div className="sech"><h2>Network interfaces</h2><span className="ln"></span>
        {h.mgmtNic && <span className="count">mgmt {h.mgmtNic} · data {(h.dataNics || []).join(", ") || "—"}</span>}</div>
      <NicTable nics={h.nics} mgmt={h.mgmtNic} data={h.dataNics} />
      <div className="dcols">
        <div className="card"><h3>Host properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Hostname", h.hostname], ["Management IP", h.mgmtIp], ["Status", <TrafficLight status={h.status} />],
            ["Zone", h.zoneId ? <Ref onClick={() => nav.openZone(h.zoneId)} label={h.zone || regName(h.zoneId, "zone")} /> : null],
            ["Region", h.region], ["Rack", h.rack], ["Cabinet", h.cabinet],
            ["Device class", h.hostClass],
            ["Control plane services", h.controlPlane ? "yes" : "no"],
            ["NUMA sockets in use", h.socketsUsed ? h.socketsUsed.join(", ") : "all"],
            ["Memory per storage-plane pod", h.memoryPerPod ? fmtBytes(h.memoryPerPod, 0) : null],
            ["Management NIC", h.mgmtNic], ["Data NICs", (h.dataNics || []).join(", ") || null],
            ["Storage nodes", h.nodeIds.length
              ? <span style={{display: "flex", gap: 8, justifyContent: "flex-end", flexWrap: "wrap"}}>{h.nodeIds.map(id => <Ref key={id} onClick={() => nav.openNode(h.clusterId, id)} label={regName(id, shortId(id))} />)}</span>
              : null],
            ["Cluster", <Ref onClick={() => nav.openCluster(h.clusterId)} label={regName(h.clusterId)} />],
            ["Prepared", fmtDate(h.preparedAt)]
          ]} /></div></div>
        <div className="card"><h3>Labels</h3><div className="bd">
          <div className="labels">{Object.entries(h.labels).map(([k, v]) => <span className="lab" key={k}><i>{k}</i>{v}</span>)}</div>
        </div></div>
      </div>
    </div>
  );
}

// The devices attached to one node, with the per-device actions in place —
// restart, health check, fail (failure migration) and remove — so a drive can
// be dealt with from the node it hangs off, not only from the devices layer.
function NodeDeviceTable({node, nav}) {
  const [rev, setRev] = useState(0);
  const {data, loading, error, reload} = useResource("ndev|" + node.id + "|" + rev, () => api.devices(node.id), 6000);
  const ds = data || [];
  const nvme = ((REG[node.clusterId] || {}).mode || (ds[0] || {}).mode) === "nvme";
  return (
    <>
      <div className="sech"><h2>Devices on this node</h2><span className="ln"></span>
        <span className="count">{ds.filter(x => x.status === "online").length} of {ds.length} online</span></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        {error ? <div className="lmsg">{error.message}</div>
          : loading && !ds.length ? <div className="lmsg">loading…</div>
          : !ds.length ? <div className="lmsg">This node has no devices attached.</div>
          : <table className="dt"><thead><tr>
              <th>Status</th><th>{nvme ? "Serial" : "Block device"}</th>
              <th>{nvme ? "PCIe" : "Serial"}</th><th>Block device</th><th>Model</th>
              <th style={{textAlign: "right"}}>Capacity</th><th>Health</th><th style={{width: 34}}></th>
            </tr></thead>
            <tbody>{ds.map(x => (
              <tr key={x.id}>
                <td><TrafficLight status={x.status} sm label /></td>
                <td><button className="tlink" onClick={() => nav.detail(x)}>{nvme ? x.serial : (x.blockdev || x.serial)}</button></td>
                <td className="mono">{(nvme ? x.pcie : x.serial) || "—"}</td>
                <td className="mono">{x.blockdev || "—"}</td>
                <td className="mono">{x.model || "—"}</td>
                <td className="mono" style={{textAlign: "right"}}>{fmtBytes(x.capacity.used)} / {fmtBytes(x.capacity.total)}</td>
                <td>{(x.status === "online" || x.status === "read_only") && x.health
                  ? <span className="lab"><Dot c={STATUS_META[x.health].c} />{STATUS_META[x.health].label}</span>
                  : <span style={{color: "var(--dim2)"}}>—</span>}</td>
                <td><ActionBtn obj={x} /></td>
              </tr>
            ))}</tbody></table>}
      </div></div>
    </>
  );
}

function NodeDetail({o: n, nav}) {
  const [tab, setTab] = useState("overview");
  return (
    <div>
      <DetailHead obj={n} title={n.hostname} badge={<span className="badge">storage node</span>}
        sub={<span className="mono" style={{fontSize: 11.5, color: "var(--dim)"}}>{n.ip}:{n.port}</span>} />
      <Tabs active={tab} onChange={setTab} items={[
        {k: "overview", label: "Overview", icon: "node"},
        {k: "threads", label: "SPDK threads", icon: "gauge"},
        {k: "logs", label: "SPDK logs", icon: "filter"}
      ]} />
      {tab === "threads" && <SpdkThreadsPanel node={n} />}
      {tab === "logs" && <NodeLogPanel node={n} />}
      {tab === "overview" && <>
      {n.status === "unreachable" && <div className="banner"><Icon n="alert" s={15} /><span><b>Node unreachable.</b> No heartbeat received. Volumes served by this node have failed over to their secondaries.</span></div>}
      {n.op && <div className="card" style={{marginBottom: 12}}><div className="bd"><NodeOpBox op={n.op} wide />
        <p className="mdesc" style={{margin: "8px 0 0"}}>{OP_HINT[n.op.kind]} Started {fmtAgo(n.op.startedAt)}; it finishes on its own — follow it under the cluster's Tasks tab.</p></div></div>}
      <div className="stats">
        <Stat k="Capacity used" v={pct(n.capacity.used, n.capacity.total).toFixed(0) + "%"} s={`${fmtBytes(n.capacity.used)} of ${fmtBytes(n.capacity.total)}`} />
        <Stat k="Devices" v={n.counts.devices} s={`${n.counts.devicesOnline} online`} />
        <Stat k="vCPU reserved" v={n.cpuReserved} s={`of ${n.cpuCount} cores on host`} />
        <Stat k="System RAM" v={fmtBytes(n.memory.used, 0)} s={`used of ${fmtBytes(n.memory.total, 0)}`} />
        <Stat k="RAM reserved" v={fmtBytes(n.memory.reserved, 0)} s="requests/limits" />
        <Stat k="Hugepages" v={fmtBytes(n.hugepages.used, 0)} s={`of ${fmtBytes(n.hugepages.total, 0)} allocated`} />
      </div>
      <div className="dcols" style={{marginTop: 12}}>
        <div className="card"><h3>Resource reservation</h3><div className="bd">
          <AllocBar label="System memory in use" used={n.memory.used} total={n.memory.total} color="var(--ok)" />
          <AllocBar label="Reserved for this node" used={n.memory.reserved} total={n.memory.total} color="var(--accent)" />
          <AllocBar label="Hugepages in use" used={n.hugepages.used} total={n.hugepages.total} color="var(--ro)" />
          <div className="kv" style={{marginTop: 10}}>
            <div><span>vCPU reserved</span><b>{n.cpuReserved}</b></div>
            <div><span>Cores on host</span><b>{n.cpuCount}</b></div>
            <div><span>SPDK</span><b>{n.spdk}</b></div>
          </div>
        </div></div>
        <IOCards o={n} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="device" title="Devices" sub="attached storage media" count={n.counts.devices} onClick={() => nav.layer(n, "devices")} />
        {n.hostId && <NavCard icon="host" title="Host" sub="machine running this node" count="→" onClick={() => nav.openHost(n.clusterId, n.hostId)} />}
      </div>
      <NodeDeviceTable node={n} nav={nav} />
      <div className="dcols">
        <div className="card"><h3>Node properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Hostname", n.hostname], ["Management IP", n.mgmtIp],
            ["Data paths", (n.dataNics || []).length
              ? <span style={{display: "flex", flexDirection: "column", gap: 2, alignItems: "flex-end"}}>
                  {n.dataNics.map(x => <span key={x.name + x.ip}>{x.name} · {x.ip}:{x.port}</span>)}
                </span>
              : n.ip],
            ["Multipathing", n.multipath ? "yes — two paths" : "no — single path"],
            ["Status", <TrafficLight status={n.status} />],
            ["Failure domain", n.failureDomain], ["Physical label", n.physicalLabel],
            ["Host", n.hostId ? <Ref onClick={() => nav.openHost(n.clusterId, n.hostId)} label={regName(n.hostId, "host")} /> : null],
            ["Cluster", <Ref onClick={() => nav.openCluster(n.clusterId)} label={regName(n.clusterId)} />],
            ["vCPU reserved", n.cpuReserved], ["Max subsystems", n.maxSubsystems], ["CPU cores", n.cpuCount],
            ["System memory", fmtBytes(n.memory.total, 0)],
            ["Memory reserved", fmtBytes(n.memory.reserved, 0)],
            ["Memory in use", fmtBytes(n.memory.used, 0)],
            ["Hugepages", `${fmtBytes(n.hugepages.used, 0)} / ${fmtBytes(n.hugepages.total, 0)}`], ["SPDK", n.spdk]
          ]} /></div></div>
        <div className="card"><h3>I/O totals</h3><div className="bd">
          <div className="stats">
            <Stat k="IOPS read" v={fmtNum(n.iops.r)} /><Stat k="IOPS write" v={fmtNum(n.iops.w)} />
            <Stat k="Read" v={fmtBW(n.bw.r)} s="GB/s" /><Stat k="Write" v={fmtBW(n.bw.w)} s="GB/s" />
          </div>
        </div></div>
      </div>
      </>}
    </div>
  );
}

function DeviceDetail({o: d, nav}) {
  const showHealth = (d.status === "online" || d.status === "read_only") && d.health;
  return (
    <div>
      <DetailHead obj={d} title={d.mode === "nvme" ? d.serial : d.blockdev} badge={<span className="badge">{d.model}</span>}
        sub={showHealth ? <span style={{fontSize: 11.5, color: "var(--dim)", display: "flex", alignItems: "center", gap: 6}}>health<TrafficLight status={d.health} /></span> : null} />
      {showHealth && d.health === "critical" && <div className="banner"><Icon n="alert" s={15} /><span><b>Media health critical.</b> Elevated error counters reported. Schedule replacement and let the cluster rebalance.</span></div>}
      {d.status === "removed" && <div className="banner" style={{color: "var(--warn)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"}}>
        <Icon n="power" s={15} /><span><b>Out of service — this is reversible.</b> The cluster still knows this device and it can be added back at any time; its chunks resynchronise from the surviving copies. Redundancy stays reduced until it returns. If the drive is not coming back, fail it so its chunks are rebuilt and fault tolerance is restored without it.</span></div>}
      {d.status === "failed" && <div className="banner" style={{color: "var(--bad)", background: "color-mix(in srgb,var(--bad) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--bad) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>Permanently failed.</b> This device is excluded from the cluster and every chunk that lived on it has been rebuilt onto the remaining devices, so fault tolerance is restored without it. It cannot be added back — replace the drive and add the new one to the node.</span></div>}
      {(d.status === "in_removal" || d.status === "in_failure") && <div className="banner" style={{color: "var(--info)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}>
        <Icon n="clock" s={15} /><span><b>{d.status === "in_removal" ? "Removal in progress." : "Failure migration in progress."}</b> {d.status === "in_removal" ? "The device is being detached from the distribution layer." : "Chunks are being rebuilt onto the remaining devices."} Follow it under the cluster's Operations tab.</span></div>}
      <div className="stats">
        <Stat k="Capacity used" v={pct(d.capacity.used, d.capacity.total).toFixed(0) + "%"} s={`${fmtBytes(d.capacity.used)} of ${fmtBytes(d.capacity.total)}`} />
        <Stat k="IOPS read" v={fmtNum(d.iops.r)} /><Stat k="IOPS write" v={fmtNum(d.iops.w)} />
        <Stat k="Temperature" v={d.temp + "°C"} c={d.temp > 60 ? "var(--warn)" : undefined} />
        <Stat k="Wear level" v={d.wear + "%"} c={d.wear > 25 ? "var(--warn)" : undefined} />
        <Stat k="Power-on" v={(d.poweronHours / 1000).toFixed(1) + "k"} s="hours" />
      </div>
      <div className="dcols">
        <div className="card"><h3>Device properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Serial number", d.serial], ["PCIe address", d.pcie], ["Block device", d.blockdev],
            ["NUMA socket", d.socket], ["Model", d.model], ["Firmware", d.firmware],
            ["Status", <TrafficLight status={d.status} />],
            showHealth ? ["Health", <TrafficLight status={d.health} />] : null,
            d.lastHealthCheck ? ["Last health check", fmtDate(d.lastHealthCheck)] : null,
            ["Storage node", <Ref onClick={() => nav.openNode(d.clusterId, d.nodeId)} label={regName(d.nodeId)} />],
            d.hostId ? ["Host", <Ref onClick={() => nav.openHost(d.clusterId, d.hostId)} label={regName(d.hostId, "host")} />] : null,
            ["Cluster", <Ref onClick={() => nav.openCluster(d.clusterId)} label={regName(d.clusterId)} />],
            ["Raw capacity", fmtBytes(d.capacity.total)]
          ]} /></div></div>
        <IOCards o={d} />
      </div>
      <div style={{marginTop: 12}}><SmartCard device={d} /></div>
    </div>
  );
}

Object.assign(window, {NodeDeviceTable, Props, Stat, Ref, NavCard, NicTable, Chart, IOCards, DetailHead, QosProps, regName, ClusterDetail, HostDetail, NodeDetail, DeviceDetail});
