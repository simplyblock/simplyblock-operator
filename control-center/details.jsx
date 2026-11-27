// A value no source reported: said so, rather than a dash that reads as "none".
const NR = ({what}) => <span className="nr" style={{color: "var(--dim2)"}} title={what ? `${what} is not reported by any source in this deployment` : undefined}>not reported</span>;
const nr = (v, what) => v === null || v === undefined || v === "" || (typeof v === "number" && !Number.isFinite(v)) ? <NR what={what} /> : v;
const nrBytes = (v, what, dp) => typeof v === "number" && Number.isFinite(v) && v > 0 ? fmtBytes(v, dp) : <NR what={what} />;
// Where a section's data came from (the control plane API, the dr-agent inventory, a draft).
const FromTag = ({what}) => <span className="srctag" title={`Reported by ${what}`}><Icon n="list" s={10} />{what}</span>;
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
      <tbody>{(nics || []).length === 0 && <tr><td colSpan={7} style={{color: "var(--dim2)"}}>No NIC is reported for this host.</td></tr>}
        {(nics || []).map(n => (
        <tr key={n.name || n.address}>
          <td className="mono" style={{fontWeight: 600}}>{n.name}</td>
          <td className="mono">{n.address}</td>
          <td className="mono" style={{color: "var(--dim)"}}>{nr(n.mac, "MAC")}</td>
          <td className="mono">{n.speed ? `${n.speed} GbE` : <NR what="link speed" />}</td>
          <td className="mono">{nr(n.socket, "NUMA socket")}</td>
          <td>{n.state ? <TrafficLight status={n.state === "up" ? "online" : "offline"} /> : <NR what="link state" />}</td>
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


function HostDetail({o: h, nav}) {
  const candidate = h.status === "discovered" || h.status === "inspecting" || h.status === "inspected";
  // devices without a reported NUMA socket are grouped under "socket not reported"
  const sockets = [...new Set([...(h.socketsUsed && h.socketsUsed.length ? h.socketsUsed : Array.from({length: h.sockets || 0}, (_, s) => s)),
    ...h.devices.map(d => d.socket === undefined ? null : d.socket)])];
  const src = h.sources || {};
  const from = (...fields) => [...new Set(fields.map(f => src[f]).filter(Boolean))].join(" + ");
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
        <Stat k="NUMA sockets" v={h.sockets ? (h.socketsUsed && h.socketsUsed.length ? `${h.socketsUsed.length} of ${h.sockets}` : h.sockets) : h.socketsUsed && h.socketsUsed.length ? h.socketsUsed.length : "not reported"}
          s={h.socketsUsed && h.socketsUsed.length ? `socket ${h.socketsUsed.join(", ")} in use` : h.sockets ? "all in use" : "no source reports the NUMA layout"} />
        <Stat k="vCPUs" v={h.vcpu || "not reported"} s={src.vcpu_count || ""} />
        <Stat k="System RAM" v={h.memory ? fmtBytes(h.memory, 0) : "not reported"} s={src.memory_total || ""} />
        <Stat k="Hugepages" v={h.hugepages.reserved ? fmtBytes(h.hugepages.reserved, 1) : "not reported"}
          s={h.hugepages.reserved ? (h.hugepages.allocated ? `${fmtBytes(h.hugepages.allocated, 1)} given to the storage plane` : "reserved") : ""} />
        <Stat k="Devices" v={`${h.counts.assigned}/${h.counts.devices}`} s={src.devices ? "assigned" : "no source reports devices"} />
        <Stat k="Raw capacity" v={fmtBytes(h.capacity.total)} s={`${fmtBytes(h.capacity.used)} claimed`} />
      </div>
      <div className="sech"><h2>Devices by NUMA socket</h2><span className="ln"></span><span className="count">{h.counts.free} unassigned</span>{src.devices && <FromTag what={src.devices} />}</div>      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Socket</th><th>Kind</th><th>PCIe</th><th>Block device</th><th>Model</th><th style={{textAlign: "right"}}>Size</th><th>Assignment</th></tr></thead>
          <tbody>
            {h.devices.length === 0 && <tr><td colSpan={7} style={{color: "var(--dim2)"}}>{src.devices ? "No device on this host." : "No source reports this host's devices."}</td></tr>}
            {sockets.map(s => h.devices.filter(d => (d.socket === undefined ? null : d.socket) === s).map(d => (
              <tr key={d.id}>
                <td className="mono">{s === null ? <NR what="NUMA socket" /> : d.socket}</td>
                <td><span className="badge">{d.kind}</span></td>
                <td className="mono">{d.pcie || "—"}</td>
                <td className="mono">{d.blockdev || "—"}</td>
                <td style={{color: "var(--dim)"}}>{d.model}</td>
                <td className="mono" style={{textAlign: "right"}}>{d.size ? fmtBytes(d.size) : <NR what="size" />}</td>
                <td>{d.selected && !d.assignedNodeId ? <span style={{color: "var(--accent)"}}>selected in the draft</span> : d.assignedNodeId
                  ? <Ref onClick={() => nav.openNode(h.clusterId, d.assignedNodeId)} label={regName(d.assignedNodeId, "storage node")} />
                  : d.reserved ? <span style={{color: "var(--warn)"}}>reserved · awaiting node restart</span>
                  : <span style={{color: "var(--dim2)"}}>unassigned</span>}</td>
              </tr>
            )))}
            {h.devices.filter(d => d.kind === "block" && !d.assignedNodeId).length === 0 && null}
          </tbody></table>
      </div></div>
      <div className="sech"><h2>Network interfaces</h2><span className="ln"></span>
        {(h.mgmtNic || (h.dataNics || []).length > 0) && <span className="count">mgmt {h.mgmtNic || "not reported"} · data {(h.dataNics || []).join(", ") || "not reported"}</span>}
        {src.nics && <FromTag what={src.nics} />}</div>
      <NicTable nics={h.nics} mgmt={h.mgmtNic} data={h.dataNics} />
      <div className="dcols">
        <div className="card"><h3>Host properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Hostname", h.hostname], ["Management IP", h.mgmtIp], ["Status", <TrafficLight status={h.status} />],
            ["Zone", h.zoneId ? <Ref onClick={() => nav.openZone(h.zoneId)} label={h.zone || regName(h.zoneId, "zone")} /> : nr(h.zone, "the zone")],
            ["Region", nr(h.region, "the region")], ["Rack", nr(h.rack, "the rack")], ["Cabinet", nr(h.cabinet, "the cabinet")],
            ["Device class", nr(h.hostClass, "the device class")],
            ["Control plane services", h.controlPlane ? "yes" : "no"],
            ["NUMA sockets in use", h.socketsUsed && h.socketsUsed.length ? h.socketsUsed.join(", ") : h.sockets ? "all" : <NR what="the NUMA layout" />],
            ["Memory per storage-plane pod", nrBytes(h.memoryPerPod, "memory per storage-plane pod", 0)],
            ["Management NIC", nr(h.mgmtNic, "the management NIC")], ["Data NICs", nr((h.dataNics || []).join(", "), "the data NICs")],
            ["Storage nodes", h.nodeIds.length
              ? <span style={{display: "flex", gap: 8, justifyContent: "flex-end", flexWrap: "wrap"}}>{h.nodeIds.map(id => <Ref key={id} onClick={() => nav.openNode(h.clusterId, id)} label={regName(id, shortId(id))} />)}</span>
              : null],
            ["Cluster", <Ref onClick={() => nav.openCluster(h.clusterId)} label={regName(h.clusterId)} />],
            ["Prepared", h.preparedAt ? fmtDate(h.preparedAt) : <NR what="the preparation time" />]
          ]} /></div></div>
        {Object.keys(src).length > 0 && <div className="card"><h3>Sources</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Devices", from("devices")], ["NICs", from("nics", "data_nics", "mgmt_nic")],
            ["vCPUs, RAM, hugepages", from("vcpu_count", "memory_total", "hugepages_reserved")],
            ["Zone, region, rack, cabinet", from("zone", "region", "rack_id", "cabinet_id")],
            ["Storage-plane memory, sockets", from("memory_per_pod", "numa_sockets_used")]
          ].map(([k, v]) => [k, v || <NR />])} /></div></div>}
        <div className="card"><h3>Labels</h3><div className="bd">
          <div className="labels">{Object.entries(h.labels).map(([k, v]) => <span className="lab" key={k}><i>{k}</i>{v}</span>)}</div>
        </div></div>
      </div>
    </div>
  );
}




Object.assign(window, {Props, Stat, Ref, NavCard, NicTable, Chart, IOCards, DetailHead, QosProps, regName, HostDetail});
