// ---------------------------------------------------------------------------
// STORAGE VIEWS: the tiles and detail pages of StorageCluster, StorageNode,
// StorageDevice, and StoragePool, drawn from the custom resources
// (storage.simplyblock.io/v1alpha2) and nothing else. Every value shown is a
// field of the object, named as the CRD names it, or a value derived from
// such fields with its formula stated next to it (a cluster's capacity is the
// sum of its nodes'; its node counts are its nodes' phases). The two-tier
// resource view (cr-view.jsx) follows each curated page, so every field the
// operator writes is one scroll away.
// ---------------------------------------------------------------------------
const PhaseTag = ({phase, status}) => (
  <span style={{display: "inline-flex", alignItems: "center", gap: 6, flexWrap: "wrap"}}>
    <TrafficLight status={phase || "Unknown"} />
    {status && <span className="lab mono" title="status.status: the control plane's own lifecycle string"><i>status</i>{status}</span>}
  </span>
);
const StepTag = ({step, claim}) => step ? <span className="lab mono" title={claim ? `status.step.claim: attempt ${claim.attempt}, lease until ${claim.leaseUntil}` : "status.step.state"}><i>step</i>{step}</span> : null;
const OpsRef = ({name, nav, vm}) => name ? <button className="lab link" title="status.activeOpsRef: the operation that holds this object's lock" onClick={e => { e.stopPropagation(); if (nav.monitor) nav.monitor({cluster: vm.kind === "cluster" ? vm.id : vm.clusterId, q: name}); }}><Icon n="clock" s={10} /><i>activeOpsRef</i>{name}</button> : null;
const Derived = ({formula}) => <span className="srctag" title={"derived: " + formula}><Icon n="gauge" s={10} />derived</span>;
const pct = (used, total) => total ? Math.round(used / total * 100) : 0;

// ---- tiles --------------------------------------------------------------------
function ClusterTile({c, nav}) {
  const cap = c.capacity;
  return (
    <div className="tile" style={{"--sc": STATUS_META[c.phase] ? STATUS_META[c.phase].c : "var(--idle)"}} onDoubleClick={() => nav.detail(c)}>
      <TileHead obj={c} left={<><PhaseTag phase={c.phase} status={c.status} /><Name>{c.name}</Name></>}
        right={<span className="badge" title="spec.clusterMode">{c.clusterMode || "Standard"}</span>} />
      <Uuid value={c.id} />
      {c.message && <div className="mdesc" title="status.message">{c.message}</div>}
      <div className="labels">
        <span className="lab" title="metadata.namespace"><i>ns</i>{c.namespace}</span>
        {c.erasureCodingScheme && <span className="lab" title="status.erasureCodingScheme"><i>EC</i>{c.erasureCodingScheme}</span>}
        {c.maxFaultTolerance !== undefined && <span className="lab" title="status.maxFaultTolerance"><i>FTT</i>{c.maxFaultTolerance}</span>}
        {c.deviceClass && <span className="lab" title="spec.deviceClass"><i>deviceClass</i>{c.deviceClass}</span>}
        <span className="lab" title="status.rebalancing"><i>rebalancing</i><Dot c={c.rebalancing ? "var(--warn)" : "var(--ok)"} />{c.rebalancing ? "true" : "false"}</span>
        <StepTag step={c.step} />
        <OpsRef name={c.activeOpsRef} nav={nav} vm={c} />
      </div>
      <div className="labels">
        <span className="lab" title="StorageNode.status.phase of the cluster's nodes" style={c.counts.nodesOnline < c.counts.nodes ? {color: "var(--warn)"} : undefined}><i>nodes Online</i>{c.counts.nodesOnline}/{c.counts.nodes}</span>
        <span className="lab" title="StorageDevice.status.phase of the cluster's devices" style={c.counts.devicesOnline < c.counts.devices ? {color: "var(--warn)"} : undefined}><i>devices Online</i>{c.counts.devicesOnline}/{c.counts.devices}</span>
        {c.tasks.length > 0 && <span className="lab" title="status.tasks: the control plane's running and pending jobs"><i>tasks</i>{c.tasks.length}</span>}
        {c.provisioningSlots.length > 0 && <span className="lab" title="status.provisioningSlots"><i>node adds</i>{c.provisioningSlots.length}</span>}
      </div>
      {cap.total > 0 && <div title={"derived: " + cap.derived}><Capacity total={cap.total} used={cap.used} /></div>}
      <Foot items={[
        {label: "Nodes", count: c.counts.nodes, icon: "node", onClick: () => nav.layer(c, "nodes")},
        {label: "Pools", count: c.counts.pools, icon: "pool", onClick: () => nav.layer(c, "pools")},
        {label: "Details", right: true, onClick: () => nav.detail(c)}
      ]} />
    </div>
  );
}

function NodeTile({n, nav}) {
  const cap = n.capacity, res = n.resources;
  return (
    <div className="tile" style={{"--sc": STATUS_META[n.phase] ? STATUS_META[n.phase].c : "var(--idle)"}} onDoubleClick={() => nav.detail(n)}>
      <TileHead obj={n} left={<><PhaseTag phase={n.phase} status={n.status} /><Name>{n.name}</Name></>}
        right={<span className="tsub" title="status.ports.management">{(n.ports || {}).management || ""}</span>} />
      <Uuid value={n.id} />
      {n.message && <div className="mdesc" title="status.message">{n.message}</div>}
      <div className="labels">
        <span className="lab" title="spec.workerNode"><i>workerNode</i>{n.workerNode}</span>
        {n.failureDomain && <span className="lab" title="status.failureDomain"><i>failureDomain</i>{n.failureDomain}</span>}
        {n.health !== undefined && n.health !== null && <span className="lab" title="status.health"><i>health</i><Dot c={n.health ? "var(--ok)" : "var(--bad)"} />{String(n.health)}</span>}
        <StepTag step={n.step} claim={n.stepClaim} />
        <OpsRef name={n.activeOpsRef} nav={nav} vm={n} />
      </div>
      {cap.total > 0 && <div title="status.resources.capacity"><Capacity total={cap.total} used={cap.used} /></div>}
      <div className="kv">
        <div title="status.resources.cpu"><span>cpu</span><b>{res.cpu !== undefined ? res.cpu : "unset"}</b></div>
        <div title="status.resources.memory"><span>memory</span><b>{res.memory || "unset"}</b></div>
        <div title="status.resources.volumes"><span>volumes</span><b>{res.volumes !== undefined ? res.volumes : "unset"}</b></div>
        <div title="status.resources.devices"><span>devices</span><b>{res.devices ? `${res.devices.online}/${res.devices.total}` : "unset"}</b></div>
      </div>
      <Foot items={[
        {label: "Devices", count: n.counts.devices, icon: "device", onClick: () => nav.layer(n, "devices")},
        {label: "Details", right: true, onClick: () => nav.detail(n)}
      ]} />
    </div>
  );
}

const verdictColor = v => v === "Ok" ? "var(--ok)" : v === "Warning" || v === "Slow" ? "var(--warn)" : v === "Failing" ? "var(--bad)" : "var(--dim2)";
function DeviceTile({d, nav}) {
  const hw = d.hardware, smart = d.health.smart || {}, perf = d.health.performance || {};
  return (
    <div className="tile" style={{"--sc": STATUS_META[d.phase] ? STATUS_META[d.phase].c : "var(--idle)"}} onDoubleClick={() => nav.detail(d)}>
      <TileHead obj={d} left={<><PhaseTag phase={d.phase} status={d.deviceStatus} /><Name>{d.name}</Name></>}
        right={<span className="badge" title="status.role">{d.role || "unset"}</span>} />
      <Uuid value={d.id} />
      {d.message && <div className="mdesc" title="status.message">{d.message}</div>}
      <div className="labels">
        <span className="lab mono" title="status.hardware.pciAddress"><i>pci</i>{hw.pciAddress || "unset"}</span>
        <span className="lab mono" title="status.hardware.serialNumber"><i>serial</i>{hw.serialNumber || "unset"}</span>
        {smart.verdict && <span className="lab" title={"status.health.smart.verdict" + (smart.reason ? ": " + smart.reason : "")}><i>smart</i><Dot c={verdictColor(smart.verdict)} />{smart.verdict}</span>}
        {perf.verdict && <span className="lab" title={"status.health.performance.verdict" + (perf.reason ? ": " + perf.reason : "")}><i>performance</i><Dot c={verdictColor(perf.verdict)} />{perf.verdict}</span>}
        <OpsRef name={d.activeOpsRef} nav={nav} vm={d} />
      </div>
      <div className="kv">
        <div title="status.capacity.totalBytes"><span>totalBytes</span><b>{d.capacity.total ? fmtBytes(d.capacity.total) : "unset"}</b></div>
        <div title="status.hardware.model"><span>model</span><b style={{fontSize: 11}}>{hw.model || "unset"}</b></div>
      </div>
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(d)}]} />
    </div>
  );
}

function PoolTile({p, nav}) {
  const lim = p.limits || {};
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.phase] ? STATUS_META[p.phase].c : "var(--idle)"}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><PhaseTag phase={p.phase} status={p.status} /><Name>{p.name}</Name></>}
        right={p.defaultStorageClassName ? <span className="badge" title="status.defaultStorageClassName">default class</span> : null} />
      <Uuid value={p.id} />
      {p.message && <div className="mdesc" title="status.message">{p.message}</div>}
      <div className="labels">
        {p.storageClassNames.map(sc => <button key={sc} className="lab link" title={"status.storageClassNames: StorageClass " + sc} onClick={e => { e.stopPropagation(); nav.openCR({kind: "StorageClass", name: sc}); }}><Icon n="k8s" s={10} />{sc}</button>)}
        {!p.storageClassNames.length && <span className="lab" style={{opacity: .6}}><i>storageClassNames</i>[]</span>}
        <OpsRef name={p.activeOpsRef} nav={nav} vm={p} />
      </div>
      <div className="kv">
        <div title="spec.limits.capacity"><span>limits.capacity</span><b>{lim.capacity || "unset"}</b></div>
        <div title="spec.limits.maxVolumeSize"><span>maxVolumeSize</span><b>{lim.maxVolumeSize || "unset"}</b></div>
        <div title="spec.limits.iops"><span>limits.iops</span><b>{lim.iops !== undefined ? fmtNum(lim.iops) : "unset"}</b></div>
        <div title="spec.volumeDefaults.filesystem"><span>filesystem</span><b>{(p.volumeDefaults || {}).filesystem || "unset"}</b></div>
      </div>
      <Foot items={[
        {label: "Volumes", icon: "volume", onClick: () => nav.layer(p, "volumes")},
        {label: "Details", right: true, onClick: () => nav.detail(p)}
      ]} />
    </div>
  );
}

// ---- details ----------------------------------------------------------------------
const TaskWindowCard = ({tasks}) => (
  <div className="card"><h3 title="StorageCluster.status.tasks">status.tasks · the control plane's running and pending jobs</h3><div className="bd" style={{padding: 0}}>
    {tasks.length ? <table className="dt"><thead><tr><th>type</th><th>status</th><th>retry</th><th>id</th></tr></thead><tbody>
      {tasks.map(t => <tr key={t.id}><td className="mono">{t.type}</td><td><span className="badge">{t.status}</span></td><td className="mono">{t.retry || 0}</td><td className="mono" style={{fontSize: 11, color: "var(--dim)"}}>{t.id}</td></tr>)}
    </tbody></table> : <div className="lmsg">No task is running or pending.</div>}
  </div></div>
);

function ClusterDetail({o: c, nav}) {
  const [tab, setTab] = useState("overview");
  const cap = c.capacity, k = c.counts;
  const items = [{k: "overview", label: "Overview", icon: "cluster"}, {k: "alerts", label: "Alerts", icon: "bell"}, {k: "ops", label: "Operations", icon: "clock"}, {k: "eventlog", label: "Event log", icon: "list"}];
  return (
    <div className="scroll">
      <DetailHead obj={Object.assign({}, c, {status: c.phase})} title={c.name}
        sub={<><span className="lab mono" title="metadata.namespace"><i>ns</i>{c.namespace}</span>{c.status && <span className="lab mono" title="status.status"><i>status</i>{c.status}</span>}<StepTag step={c.step} /><OpsRef name={c.activeOpsRef} nav={nav} vm={c} />{window.AlertBadge && <AlertBadge obj={c} nav={nav} />}</>}
        badge={c.message ? <span className="mdesc" title="status.message">{c.message}</span> : null} />
      <Tabs items={items} active={tab} onChange={setTab} />
      {tab === "overview" && <>
        <div className="stats">
          <Stat k="status.phase" v={c.phase} c={STATUS_META[c.phase] ? STATUS_META[c.phase].c : undefined} s={c.status ? "status.status " + c.status : null} />
          <Stat k="nodes Online" v={`${k.nodesOnline}/${k.nodes}`} c={k.nodesOnline < k.nodes ? "var(--warn)" : undefined} s={[k.nodesProvisioning && `${k.nodesProvisioning} Provisioning`, k.nodesOffline && `${k.nodesOffline} Offline`, k.nodesDegraded && `${k.nodesDegraded} Degraded`, k.nodesRemoving && `${k.nodesRemoving} Removing`].filter(Boolean).join(" · ") || "StorageNode.status.phase"} />
          <Stat k="devices Online" v={`${k.devicesOnline}/${k.devices}`} c={k.devicesOnline < k.devices ? "var(--warn)" : undefined} s="StorageDevice.status.phase" />
          <div className="stat"><div className="k">capacity <Derived formula={cap.derived} /></div>
            {cap.total ? <><div className="v md">{pct(cap.used, cap.total)}%</div><div className="s">{fmtBytes(cap.used)} of {fmtBytes(cap.total)}</div></> : <><div className="v sm" style={{color: "var(--dim2)"}}>unset</div><div className="s">no node reports capacity yet</div></>}</div>
          <Stat k="status.erasureCodingScheme" v={c.erasureCodingScheme || "unset"} s={c.maxFaultTolerance !== undefined ? `maxFaultTolerance ${c.maxFaultTolerance}` : null} />
          <Stat k="status.tasks" v={c.tasks.length} s={c.tasks.length ? c.tasks.map(t => t.type).slice(0, 3).join(", ") : "none running or pending"} />
          <Stat k="status.rebalancing" v={c.rebalancing ? "true" : "false"} c={c.rebalancing ? "var(--warn)" : undefined} s={c.volumeMoveGeneration !== undefined ? `volumeMoveGeneration ${c.volumeMoveGeneration}` : null} />
        </div>
        <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
        <div className="navcards">
          <NavCard icon="node" title="Storage nodes" sub="StorageNode objects with spec.clusterRef naming this cluster" count={k.nodes} onClick={() => nav.layer(c, "nodes")} />
          <NavCard icon="device" title="Devices" sub="StorageDevice objects of those nodes" count={k.devices} onClick={() => nav.layer(c, "devices")} />
          <NavCard icon="pool" title="Storage pools" sub="StoragePool objects with spec.clusterRef naming this cluster" count={k.pools} onClick={() => nav.layer(c, "pools")} />
          <NavCard icon="volume" title="Logical volumes" sub="PersistentVolumes provisioned by this cluster" count="" onClick={() => nav.layer(c, "volumes")} />
          <NavCard icon="camera" title="Snapshots" sub="VolumeSnapshots of those volumes" count="" onClick={() => nav.layer(c, "snapshots")} />
          <NavCard icon="cloud" title="Backups" sub="StorageBackup objects" count="" onClick={() => nav.layer(c, "backups")} />
          <NavCard icon="clock" title="Backup policies" sub="StorageBackupPolicy objects" count="" onClick={() => nav.layer(c, "policies")} />
        </div>
        {(c.tasks.length > 0 || c.provisioningSlots.length > 0) && <div className="dcols" style={{marginTop: 12}}>
          <TaskWindowCard tasks={c.tasks} />
          {c.provisioningSlots.length > 0 && <div className="card"><h3 title="StorageCluster.status.provisioningSlots">status.provisioningSlots</h3><div className="bd" style={{padding: 0}}>
            <table className="dt"><thead><tr><th>worker</th><th>node</th><th>takenAt</th></tr></thead><tbody>{c.provisioningSlots.map(s => <tr key={s.worker}><td className="mono">{s.worker}</td><td className="mono">{s.node}</td><td className="mono">{fmtAgo(s.takenAt)}</td></tr>)}</tbody></table></div></div>}
        </div>}
        <CRSection vm={c} nav={nav} />
      </>}
      {tab === "alerts" && (window.AlertsScoped ? <AlertsScoped cluster={c} nav={nav} /> : <ComingView name="The alert view" />)}
      {tab === "ops" && <OperationsPanel cluster={c} nav={nav} />}
      {tab === "eventlog" && <ClusterLogPanel cluster={c} />}
    </div>
  );
}

function NodeDetail({o: n, nav}) {
  const [tab, setTab] = useState("overview");
  const res = n.resources, ports = n.ports, cap = n.capacity;
  // the StorageNode CRD's field markers decide which spec.config fields the card shows (cr-view.jsx)
  // FIXME: remove together with the spec.config card below, which is all this
  // CRD lookup serves.
  const schema = window.useCrdSchema ? useCrdSchema("StorageNode") : null;
  const items = [{k: "overview", label: "Overview", icon: "node"}, {k: "logs", label: "Storage plane logs", icon: "list"}];
  return (
    <div className="scroll">
      <DetailHead obj={Object.assign({}, n, {status: n.phase})} title={n.name}
        sub={<><button className="lab link" title="spec.clusterRef" onClick={() => nav.openCluster(n.clusterId)}><Icon n="cluster" s={10} />{n.clusterRef}</button>
          <span className="lab mono" title="spec.workerNode"><i>workerNode</i>{n.workerNode}</span>{n.status && <span className="lab mono" title="status.status"><i>status</i>{n.status}</span>}<StepTag step={n.step} claim={n.stepClaim} /><OpsRef name={n.activeOpsRef} nav={nav} vm={n} /></>}
        badge={n.message ? <span className="mdesc" title="status.message">{n.message}</span> : null} />
      <Tabs items={items} active={tab} onChange={setTab} />
      {tab === "overview" && <>
        <div className="stats">
          <Stat k="status.phase" v={n.phase} c={STATUS_META[n.phase] ? STATUS_META[n.phase].c : undefined} s={n.status ? "status.status " + n.status : null} />
          <Stat k="status.health" v={n.health === undefined || n.health === null ? "unset" : String(n.health)} c={n.health === false ? "var(--bad)" : undefined} />
          <Stat k="resources.devices" v={res.devices ? `${res.devices.online}/${res.devices.total}` : "unset"} s="online / total" />
          <Stat k="resources.volumes" v={res.volumes !== undefined ? res.volumes : "unset"} />
          <div className="stat"><div className="k">resources.capacity</div>
            {cap.total ? <><div className="v md">{pct(cap.used, cap.total)}%</div><div className="s">{fmtBytes(cap.used)} of {fmtBytes(cap.total)}{cap.sampledAt ? ` · sampled ${fmtAgo(cap.sampledAt)}` : ""}</div></> : <div className="v sm" style={{color: "var(--dim2)"}}>unset</div>}</div>
          <Stat k="resources.cpu" v={res.cpu !== undefined ? res.cpu : "unset"} s={res.memory ? `memory ${res.memory}` : null} />
          <Stat k="status.uptime" v={n.uptime || "unset"} />
        </div>
        <div className="dcols">
          <div className="card"><h3>status.ports</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={[["management", ports.management], ["nvmeof", ports.nvmeof], ["rpc", ports.rpc], ["lvol", ports.lvol]].map(([k, v]) => [<span className="mono">{k}</span>, v === undefined ? null : <span className="mono">{String(v)}</span>])} /></div></div>
          {/* FIXME: remove this card. Its rows are a hand-written list, so a field
              added to the API never appears here and a removed one stays (this
              one still lists pcieAllowList), which the isInvisible filter cannot
              catch. The CRSection below already shows spec.config from the CRD,
              in order and with the field markers applied. */}
          <div className="card"><h3>spec.config</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={[
            ["mgmtInterface", n.config.mgmtInterface], ["dataInterfaces", (n.config.dataInterfaces || []).join(", ")], ["pcieAllowList", (n.config.pcieAllowList || []).join(", ")],
            ["failureDomain", n.config.failureDomain], ["site", n.config.site], ["expand", n.config.expand === undefined ? null : String(n.config.expand)]
            ].filter(([k]) => !(schema && isInvisible(schema, "spec.config." + k))).map(([k, v]) => [<span className="mono">{k}</span>, v === undefined || v === null || v === "" ? null : <span className="mono">{String(v)}</span>])} /></div></div>
        </div>
        {n.network.length > 0 && <div className="card" style={{marginTop: 12}}><h3>status.network</h3><div className="bd" style={{padding: 0}}>
          <table className="dt"><thead><tr><th>network</th><th>phase</th><th>address</th><th>assigned</th><th>interface</th><th>hostReachable</th></tr></thead><tbody>
            {n.network.map(x => <tr key={x.network}><td className="mono">{x.network}</td><td><TrafficLight status={x.phase === "Attached" ? "Online" : x.phase === "Pending" ? "Pending" : "Failed"} label={x.phase} /></td><td className="mono">{x.address || "unset"}</td><td className="mono">{x.assigned || "unset"}</td><td className="mono">{x.interface || "unset"}</td><td className="mono">{x.hostReachable === undefined ? "unset" : String(x.hostReachable)}</td></tr>)}
          </tbody></table></div></div>}
        <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
        <div className="navcards">
          <NavCard icon="device" title="Devices" sub="StorageDevice objects with spec.nodeRef naming this node" count={n.counts.devices} onClick={() => nav.layer(n, "devices")} />
          <NavCard icon="host" title="Worker node" sub={"the Kubernetes Node " + n.workerNode} count="" onClick={() => nav.openCR({kind: "Node", name: n.workerNode})} />
        </div>
        <CRSection vm={n} nav={nav} />
      </>}
      {tab === "logs" && <StoragePlaneLogPanel node={n} />}
    </div>
  );
}

function DeviceDetail({o: d, nav}) {
  const hw = d.hardware, smart = d.health.smart || {}, perf = d.health.performance || {};
  const num = v => v === undefined || v === null ? null : <span className="mono">{String(v)}</span>;
  return (
    <div className="scroll">
      <DetailHead obj={Object.assign({}, d, {status: d.phase})} title={d.name}
        sub={<><button className="lab link" title="spec.nodeRef" onClick={() => nav.openNode(d.clusterId, d.nodeId)}><Icon n="node" s={10} />{d.nodeRef}</button>
          {d.deviceStatus && <span className="lab mono" title="status.deviceStatus"><i>deviceStatus</i>{d.deviceStatus}</span>}<span className="lab" title="status.role"><i>role</i>{d.role || "unset"}</span><OpsRef name={d.activeOpsRef} nav={nav} vm={d} /></>}
        badge={d.message ? <span className="mdesc" title="status.message">{d.message}</span> : null} />
      <div className="stats">
        <Stat k="status.phase" v={d.phase} c={STATUS_META[d.phase] ? STATUS_META[d.phase].c : undefined} s={d.deviceStatus ? "deviceStatus " + d.deviceStatus : null} />
        <Stat k="capacity.totalBytes" v={d.capacity.total ? fmtBytes(d.capacity.total) : "unset"} />
        <Stat k="health.smart.verdict" v={smart.verdict || "unset"} c={verdictColor(smart.verdict)} s={smart.reason} />
        <Stat k="health.performance.verdict" v={perf.verdict || "unset"} c={verdictColor(perf.verdict)} s={perf.reason} />
      </div>
      <div className="dcols">
        <div className="card"><h3>status.hardware</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={[["model", hw.model], ["serialNumber", hw.serialNumber], ["pciAddress", hw.pciAddress], ["nvmeController", hw.nvmeController]].map(([k, v]) => [<span className="mono">{k}</span>, num(v)])} /></div></div>
        <div className="card"><h3>status.health.smart</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={[["checkedAt", smart.checkedAt ? fmtAgo(smart.checkedAt) : null], ["source", smart.source], ["temperatureCelsius", smart.temperatureCelsius], ["percentageUsed", smart.percentageUsed],
          ["availableSparePercent", smart.availableSparePercent], ["availableSpareThresholdPercent", smart.availableSpareThresholdPercent], ["mediaErrors", smart.mediaErrors], ["criticalWarning", smart.criticalWarning], ["powerOnHours", smart.powerOnHours], ["unsafeShutdowns", smart.unsafeShutdowns]].map(([k, v]) => [<span className="mono">{k}</span>, num(v)])} /></div></div>
      </div>
      <div className="dcols" style={{marginTop: 12}}>
        <div className="card"><h3>status.health.performance</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={[["assessedAt", perf.assessedAt ? fmtAgo(perf.assessedAt) : null], ["since", perf.since ? fmtAgo(perf.since) : null], ["latencyMicroseconds", perf.latencyMicroseconds], ["fleetLatencyMicroseconds", perf.fleetLatencyMicroseconds], ["latencyRatioPercent", perf.latencyRatioPercent], ["iopsRatioPercent", perf.iopsRatioPercent]].map(([k, v]) => [<span className="mono">{k}</span>, num(v)])} /></div></div>
        <div className="card"><h3>identity</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={[["spec.deviceID", d.id], ["status.clusterID", d.clusterId], ["status.nodeID", d.nodeId], ["status.site", d.site], ["status.remote", d.remote === undefined ? null : String(d.remote)]].map(([k, v]) => [<span className="mono">{k}</span>, num(v)])} /></div></div>
      </div>
      <CRSection vm={d} nav={nav} />
    </div>
  );
}

function PoolDetail({o: p, nav}) {
  const lim = p.limits || {}, vd = p.volumeDefaults || {}, sl = p.statusLimits || {};
  const num = v => v === undefined || v === null || v === "" ? null : <span className="mono">{typeof v === "object" ? JSON.stringify(v) : String(v)}</span>;
  return (
    <div className="scroll">
      <DetailHead obj={Object.assign({}, p, {status: p.phase})} title={p.name}
        sub={<><button className="lab link" title="spec.clusterRef" onClick={() => nav.openCluster(p.clusterId)}><Icon n="cluster" s={10} />{p.clusterRef}</button>{p.status && <span className="lab mono" title="status.status"><i>status</i>{p.status}</span>}<OpsRef name={p.activeOpsRef} nav={nav} vm={p} /></>}
        badge={p.message ? <span className="mdesc" title="status.message">{p.message}</span> : null} />
      <div className="stats">
        <Stat k="status.phase" v={p.phase} c={STATUS_META[p.phase] ? STATUS_META[p.phase].c : undefined} s={p.status ? "status.status " + p.status : null} />
        <Stat k="spec.limits.capacity" v={lim.capacity || "unset"} s={sl.capacity ? `status.limits.capacity ${sl.capacity}` : null} />
        <Stat k="spec.limits.iops" v={lim.iops !== undefined ? fmtNum(lim.iops) : "unset"} s={sl.iops !== undefined ? `status.limits.iops ${fmtNum(sl.iops)}` : null} />
        <Stat k="status.storageClassNames" v={p.storageClassNames.length} s={p.defaultStorageClassName ? `default ${p.defaultStorageClassName}` : null} />
      </div>
      <div className="dcols">
        <div className="card"><h3>spec.volumeDefaults</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={Object.entries(vd).map(([k, v]) => [<span className="mono">{k}</span>, num(v)])} />{!Object.keys(vd).length && <div className="nolim">unset</div>}</div></div>
        <div className="card"><h3>status.limits</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}><Props rows={Object.entries(sl).map(([k, v]) => [<span className="mono">{k}</span>, num(v)])} />{!Object.keys(sl).length && <div className="nolim">unset</div>}</div></div>
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="volume" title="Logical volumes" sub="PersistentVolumes whose class is assigned to this pool" count="" onClick={() => nav.layer(p, "volumes")} />
        {p.storageClassNames.map(sc => <NavCard key={sc} icon="k8s" title={sc} sub="StorageClass assigned to this pool (status.storageClassNames)" count="" onClick={() => nav.openCR({kind: "StorageClass", name: sc})} />)}
      </div>
      <CRSection vm={p} nav={nav} />
    </div>
  );
}

Object.assign(window, {ClusterTile, NodeTile, DeviceTile, PoolTile, ClusterDetail, NodeDetail, DeviceDetail, PoolDetail, PhaseTag, StepTag, OpsRef, Derived, TaskWindowCard});
