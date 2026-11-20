const Dot = ({c}) => <i style={{width: 8, height: 8, borderRadius: "50%", background: c, boxShadow: `0 0 0 3px color-mix(in srgb,${c} 20%,transparent)`, flex: "none", display: "block"}}></i>;

const TileHead = ({obj, left, right}) => (
  <div className="th">
    <div style={{minWidth: 0, flex: 1}}>{left}</div>
    <div style={{display: "flex", alignItems: "center", gap: 6, flex: "none"}}>{right}<ActionBtn obj={obj} /></div>
  </div>
);
const Name = ({children}) => <div className="tname" title={children}>{children}</div>;

const Foot = ({items}) => (
  <div className="tfoot">{items.filter(Boolean).map((it, i) => (
    <button key={i} className={"fbtn" + (it.right ? " det" : "")} onClick={e => {e.stopPropagation(); it.onClick();}}>
      {it.icon && <Icon n={it.icon} s={13} />}<span>{it.label}</span>{it.count !== undefined && <b>{it.count}</b>}
      {!it.right && <Icon n="chev" s={11} />}
    </button>
  ))}</div>
);

const IoMetrics = ({o}) => (
  <div className="mets">
    <Metric label="IOPS" r={o.iops.r} w={o.iops.w} fmt={fmtNum} hist={o.hist.iops} color="var(--accent)" />
    <Metric label="Throughput" unit="GB/s" r={o.bw.r} w={o.bw.w} fmt={fmtBW} hist={o.hist.bw} color="var(--ok)" />
  </div>
);


const HOST_CANDIDATE = {discovered: 1, inspecting: 1, inspected: 1};

function HostTile({h, nav, select}) {
  if (HOST_CANDIDATE[h.status]) return <HostCandidateTile h={h} nav={nav} select={select} />;
  // a configured host only puts the sockets it was given into service
  const inUse = h.socketsUsed && h.socketsUsed.length ? h.socketsUsed : Array.from({length: h.sockets || 0}, (_, s) => s);
  const bySocket = inUse.map(s => {
    const d = h.devices.filter(x => x.kind === "nvme" && x.socket === s);
    return {s, total: d.length, free: d.filter(x => !x.assignedNodeId).length};
  }).filter(x => x.total > 0);
  return (
    <div className="tile" style={{"--sc": STATUS_META[h.status].c}} onDoubleClick={() => nav.detail(h)}>
      <TileHead obj={h} left={<><TrafficLight status={h.status} /><Name>{h.hostname}</Name></>}
        right={<span className="tsub">{h.mgmtIp}</span>} />
      <Uuid value={h.id} />
      <div className="labels">
        {h.controlPlane && <span className="lab" style={{color: "var(--accent)", borderColor: "var(--accent-line)"}}><Icon n="cluster" s={10} />control plane</span>}
        <span className="lab"><i>nodes</i>{h.counts.nodes || "none"}</span>
        <span className="lab"><i>sockets</i>{h.socketsUsed && h.socketsUsed.length ? `${h.socketsUsed.length} of ${h.sockets}` : h.sockets}</span>
        {h.hostClass && <span className="lab"><i>class</i>{h.hostClass}</span>}
      </div>
      <div className="labels">
        {h.zoneId && <button className="lab link" onClick={e => {e.stopPropagation(); nav.openZone(h.zoneId);}}><Icon n="zone" s={10} />{regName(h.zoneId, "zone")}</button>}
        {h.rack && <span className="lab"><i>rack</i>{h.rack}</span>}
        {h.cabinet && <span className="lab"><i>cabinet</i>{h.cabinet}</span>}
        {h.migrationTaint && <span className="lab" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}} title={h.migrationTaint}><Icon n="move" s={10} />migration target</span>}
        {h.mgmtNic && <span className="lab"><i>mgmt</i>{h.mgmtNic}</span>}
        {h.dataNics && h.dataNics.length > 0 && <span className="lab"><i>data</i>{h.dataNics.join(", ")}</span>}
      </div>
      <div className="sockets">
        {bySocket.map(s => (
          <div className="sock" key={s.s}>
            <span className="sk">NUMA {s.s}</span>
            <span className="sv">{s.total - s.free}<em>/{s.total}</em> nvme</span>
            <div className="pips">{Array.from({length: s.total}).map((_, i) => <i key={i} className={i < s.total - s.free ? "on" : ""}></i>)}</div>
          </div>
        ))}
        {h.counts.blockFree > 0 && <div className="sock"><span className="sk">unused</span><span className="sv">{h.counts.blockFree} free block dev</span></div>}
      </div>
      <div className="mets">
        <div className="met"><div className="k"><span>Compute</span></div>
          <div className="rw"><div><span>VCPU</span><b>{h.vcpu}</b></div><div><span>RAM</span><b>{fmtBytes(h.memory, 0)}</b></div></div></div>
        <div className="met"><div className="k"><span>Hugepages</span></div>
          <div className="rw"><div><span>ALLOC</span><b>{fmtBytes(h.hugepages.allocated, 0)}</b></div><div><span>RESERVED</span><b>{fmtBytes(h.hugepages.reserved, 0)}</b></div></div></div>
      </div>
      {h.counts.nodes === 0 && <div className="nolim" style={{color: "var(--accent)"}}>Prepared &amp; labelled — no storage node yet</div>}
      <Foot items={[
        h.counts.nodes > 0 && {label: "Nodes", count: h.counts.nodes, icon: "node", onClick: () => nav.hostNodes(h)},
        {label: "Details", right: true, onClick: () => nav.detail(h)}
      ]} />
    </div>
  );
}

function HostCandidateTile({h, nav, select}) {
  const selectable = h.status === "discovered" && select;
  const on = selectable && select.has(h.id);
  const nvme = h.devices.filter(d => d.kind === "nvme").length;
  return (
    <div className={"tile candidate" + (on ? " sel" : "")} style={{"--sc": STATUS_META[h.status].c}}
      onDoubleClick={() => nav.detail(h)}>
      <TileHead obj={h}
        left={<div style={{display: "flex", alignItems: "flex-start", gap: 9}}>
          {selectable && <button className={"selbox" + (on ? " on" : "")} title="Select for preparation"
            onClick={e => {e.stopPropagation(); select.toggle(h.id);}}>{on && <Icon n="check" s={10} />}</button>}
          <div style={{minWidth: 0}}><TrafficLight status={h.status} /><Name>{h.hostname}</Name></div>
        </div>}
        right={<span className="badge k8s">k8s worker</span>} />
      <Uuid value={h.id} />
      <div className="labels">
        <span className="lab"><i>kubelet</i>{h.kubelet}</span>
        {h.zoneId && <button className="lab link" onClick={e => {e.stopPropagation(); nav.openZone(h.zoneId);}}><Icon n="zone" s={10} />{regName(h.zoneId, "zone")}</button>}
        {h.rack && <span className="lab"><i>rack</i>{h.rack}</span>}
        {h.k8sLabels["node.kubernetes.io/instance-type"] && <span className="lab"><i>type</i>{h.k8sLabels["node.kubernetes.io/instance-type"]}</span>}
        {h.k8sLabels["topology.kubernetes.io/zone"] && <span className="lab"><i>zone</i>{h.k8sLabels["topology.kubernetes.io/zone"]}</span>}
      </div>
      {h.status === "discovered" && <div className="prepbox">
        Not prepared. Deploying the inspection pod collects the NUMA topology, devices and NICs.
      </div>}
      {h.status === "inspecting" && <div className="prepbox running">
        <span className="dots"><i></i><i></i><i></i></span>
        {h.inspection ? h.inspection.pod : "inspection pod"} — collecting device and NUMA inventory…
      </div>}
      {h.status === "inspected" && <>
        <div className="sockets">
          {Array.from({length: h.sockets}, (_, s) => (
            <div className="sock" key={s}>
              <span className="sk">NUMA {s}</span>
              <span className="sv">{h.devices.filter(d => d.socket === s).length} dev · {h.nics.filter(n => n.socket === s).length} nic</span>
            </div>
          ))}
        </div>
        <div className="prepbox ready">Inventory collected — {nvme} NVMe, {h.devices.length - nvme} block, {h.nics.length} NIC(s). Choose sockets, memory, devices and NICs to finish.</div>
      </>}
      <div className="mets">
        <div className="met"><div className="k"><span>Node capacity</span></div>
          <div className="rw"><div><span>VCPU</span><b>{h.vcpu}</b></div><div><span>RAM</span><b>{fmtBytes(h.memory, 0)}</b></div></div></div>
        <div className="met"><div className="k"><span>Inventory</span></div>
          <div className="rw"><div><span>DEVICES</span><b>{h.sockets ? h.devices.length : "—"}</b></div><div><span>NICS</span><b>{h.nics.length || "—"}</b></div></div></div>
      </div>
      <Foot items={[
        h.status === "discovered" ? {label: "Prepare", icon: "plus", right: true,
          onClick: () => window.__ui.dialog(ACTIONS.host(h)[0].dialog, h)} : null,
        h.status === "inspected" ? {label: "Configure", icon: "gauge", right: true,
          onClick: () => window.__ui.dialog(configureHostDialog(h), h)} : null,
        h.status === "inspecting" ? {label: "Details", right: true, onClick: () => nav.detail(h)} : null
      ]} />
    </div>
  );
}

// phase tracker for a running node operation
const OP_LABEL = {removal: "Removal", expansion: "Expansion", migration: "Migration"};
const OP_HINT = {
  removal: "Data is rebalanced onto the remaining nodes, then the volumes whose primary sits here are moved off; the node and its device records are deleted at the end.",
  expansion: "The storage node is deployed and joins the cluster, then existing data is rebalanced onto it.",
  migration: "The node restarts on the prepared target host, data is rebalanced, and the record on the old host is removed."
};
function NodeOpBox({op, wide}) {
  return (
    <div className="prepbox running">
      <div className="opl"><span className="dots"><i></i><i></i><i></i></span>
        <b>{OP_LABEL[op.kind] || op.kind}</b>
        <span>{op.phase}</span>
        {op.kind === "migration" && op.targetHostname && <span className="mono">→ {op.targetHostname}</span>}
        {op.kind === "removal" && !!op.volumesMoved && <span className="mono">{op.volumesMoved} volume(s) moved off</span>}
      </div>
      <div className="opph">{op.phases.map((p, i) => (
        <span key={p} className={"opp" + (i < op.phaseIndex ? " done" : i === op.phaseIndex ? " on" : "")} title={p}>
          {wide && <i>{p}</i>}
        </span>
      ))}</div>
    </div>
  );
}



Object.assign(window, {NodeOpBox, OP_HINT, Dot, TileHead, Name, Foot, IoMetrics, HostTile, HostCandidateTile});
